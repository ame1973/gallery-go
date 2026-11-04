package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func TestJobResourceMigrationBackfillsPrimaryKeysAndKeepsTerminalHistory(t *testing.T) {
	db := testDB(t)
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "0011_" {
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
	if _, err := db.Exec(`INSERT INTO jobs(id,kind,resource_key,payload_json,status,next_run_at,created_at,updated_at) VALUES('queued','scan','source:one','{}','queued',0,0,0),('terminal','scan','source:two','{}','cancelled',0,0,0),('unbound','scan',NULL,'{}','failed',0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_resources`).Scan(&count); err != nil || count != 2 {
		t.Fatal("resource migration lost primary/history rows", count, err)
	}
	if _, err := db.Exec(`INSERT INTO job_resources VALUES('unknown-job','source:one')`); err == nil {
		t.Fatal("orphan job binding accepted")
	}
	if _, err := db.Exec(`INSERT INTO job_resources VALUES('queued','')`); err == nil {
		t.Fatal("empty binding accepted")
	}
	if _, err := db.Exec(`INSERT INTO job_resources VALUES('queued','source:one')`); err == nil {
		t.Fatal("duplicate dependency accepted")
	}
	if _, err := db.Exec(`DELETE FROM jobs WHERE id='queued'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_resources WHERE job_id='terminal'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("deleting another job erased terminal dependency history", count, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM job_resources WHERE job_id='queued'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("job cleanup did not cascade private dependencies", count, err)
	}
}
