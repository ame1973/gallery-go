package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func TestScanTargetMigrationNeverInventsScopeAndPreservesPrivateHistory(t *testing.T) {
	db := testDB(t)
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "0012_" {
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
	if _, err := db.Exec(`INSERT INTO jobs(id,kind,resource_key,payload_json,status,next_run_at,created_at,updated_at) VALUES('old-job','scan','scan:all','{}','queued',0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scan_targets`).Scan(&count); err != nil || count != 0 {
		t.Fatal("migration fabricated an unpinned target selection", count, err)
	}
	for _, statement := range []string{
		`INSERT INTO scan_targets VALUES('unknown-job','source',1,'.',NULL)`,
		`INSERT INTO scan_targets VALUES('old-job','source',0,'.',NULL)`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatal("invalid private target accepted", statement)
		}
	}
	if _, err := db.Exec(`INSERT INTO scan_targets VALUES('old-job','deleted-registration',1,'.',NULL)`); err != nil {
		t.Fatal("history must not depend on a live source registration", err)
	}
	if _, err := db.Exec(`INSERT INTO scan_targets VALUES('old-job','deleted-registration',2,'Album',NULL)`); err == nil {
		t.Fatal("one immutable source scope per job was not enforced")
	}
	if _, err := db.Exec(`DELETE FROM jobs WHERE id='old-job'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM scan_targets`).Scan(&count); err != nil || count != 0 {
		t.Fatal("job cleanup did not cascade private targets", count, err)
	}
}
