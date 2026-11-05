package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func TestJobIssueMigrationBackfillsAndRetainsCommittedHistoryAfterSourceRemoval(t *testing.T) {
	db := testDB(t)
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "0013_" {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: data}
	}
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO libraries(id,name,created_at) VALUES('library','Isolated diagnostic migration',0);
		INSERT INTO sources(id,library_id,name,root_path,mapping_json,created_at) VALUES('source','library','fixture','/never-opened-migration-fixture','{}',0);
		INSERT INTO jobs(id,kind,payload_json,status,attempts,next_run_at,created_at,updated_at) VALUES('job','scan','{}','running',1,0,0,0);
		INSERT INTO inventory_generations(id,source_id,job_id,attempt,lease_owner,config_version,generation,mode,state,base_counts_json,created_at,updated_at) VALUES('generation','source','job',1,'never-public-owner',7,1,'full','failed','{}',0,0);
		INSERT INTO inventory_issues VALUES('first','generation','Album/bad.jpg','INVALID_MEDIA',123);
	`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var job, source, generation, path, code string
	var attempt int
	var config, created int64
	if err := db.QueryRow(`SELECT job_id,attempt,source_id,source_config_version,generation_id,relative_path,error_code,created_at FROM job_issues WHERE id='first'`).Scan(&job, &attempt, &source, &config, &generation, &path, &code, &created); err != nil || job != "job" || attempt != 1 || source != "source" || config != 7 || generation != "generation" || path != "Album/bad.jpg" || code != "INVALID_MEDIA" || created != 123 {
		t.Fatal("backfill invented or discarded historical diagnostic context", job, attempt, source, config, generation, path, code, created, err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO inventory_issues VALUES('rolled-back','generation','Album/second.jpg','CODEC_UNAVAILABLE',456)`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM job_issues`).Scan(&count); err != nil || count != 2 {
		t.Fatal("projection did not share the staging transaction", count, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_issues`).Scan(&count); err != nil || count != 1 {
		t.Fatal("rolled-back diagnostic escaped", count, err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_issue_projection BEFORE INSERT ON job_issues BEGIN SELECT RAISE(ABORT,'injected projection failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO inventory_issues VALUES('fault','generation','Album/fault.jpg','MEDIA_LIMIT',789)`); err == nil {
		t.Fatal("private issue committed without its public-safe history projection")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM inventory_issues`).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed projection left a partial private issue", count, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_issue_projection; DELETE FROM sources WHERE id='source'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM inventory_generations)+(SELECT COUNT(*) FROM inventory_issues)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("private source staging did not cascade", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_issues WHERE source_id='source' AND source_config_version=7 AND generation_id='generation'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("source deletion erased committed job diagnostics", count, err)
	}
	if _, err := db.Exec(`DELETE FROM jobs WHERE id='job'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_issues`).Scan(&count); err != nil || count != 0 {
		t.Fatal("job history was not the diagnostic owner", count, err)
	}
}
