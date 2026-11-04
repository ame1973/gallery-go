package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func TestMetadataMigrationPreservesSealedInventoryAndRequiresNewEvidence(t *testing.T) {
	db := testDB(t)
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "0009_" {
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
		INSERT INTO libraries(id,name,created_at) VALUES('library','Isolated migration fixture',0);
		INSERT INTO sources(id,library_id,name,root_path,mapping_json,created_at) VALUES('source','library','fixture','/never-opened-migration-fixture','{}',0);
		INSERT INTO jobs(id,kind,payload_json,status,next_run_at,created_at,updated_at) VALUES('job','scan','{}','running',0,0,0);
		INSERT INTO inventory_generations(id,source_id,job_id,attempt,lease_owner,config_version,generation,mode,state,healthy,file_count,size_bytes,base_counts_json,created_at,updated_at,completed_at) VALUES('generation','source','job',1,'private-test-owner',1,1,'full','ready',1,1,3,'{}',0,0,0);
	`); err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("a", 64)
	if _, err := db.Exec(`INSERT INTO inventory_files(generation_id,relative_path,size_bytes,mtime_ns,file_identity,quick_fingerprint,sha256) VALUES('generation','photo.png',3,0,'1:1',?,?)`, digest, digest); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var count int
	var complete bool
	var policy, state string
	if err := db.QueryRow(`SELECT metadata_count,metadata_complete,inspect_policy_sha256,state FROM inventory_generations WHERE id='generation'`).Scan(&count, &complete, &policy, &state); err != nil || count != 0 || complete || policy != "" || state != "ready" {
		t.Fatal("old filesystem readiness invented metadata evidence", count, complete, policy, state, err)
	}
	for _, statement := range []string{
		`UPDATE inventory_generations SET metadata_count=2 WHERE id='generation'`,
		`UPDATE inventory_generations SET metadata_complete=1 WHERE id='generation'`,
		`INSERT INTO inventory_media VALUES('generation','unknown.png',1,'ok','image','image/png','{}')`,
		`INSERT INTO inventory_media VALUES('generation','photo.png',1,'ok','image','image/png','not JSON')`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatal("invalid metadata relationship accepted", statement)
		}
	}
	if _, err := db.Exec(`INSERT INTO inventory_media VALUES('generation','photo.png',1,'ok','image','image/png','{}'); DELETE FROM inventory_generations WHERE id='generation'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM inventory_files)+(SELECT COUNT(*) FROM inventory_media)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("private staging did not cascade", count, err)
	}
}
