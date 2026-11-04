package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func TestAccessMigrationPreservesAndLatchesRestrictions(t *testing.T) {
	db := testDB(t)
	old := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "000[1-4]_*.sql")
	if err != nil || len(names) != 4 {
		t.Fatalf("pre-access migrations: %v %v", names, err)
	}
	for _, name := range names {
		data, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		old[name] = &fstest.MapFile{Data: data}
	}
	if err := Migrate(t.Context(), db, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO permission_groups(id,name,created_at) VALUES('group','Family',0);
	 INSERT INTO libraries(id,name,created_at) VALUES('library','Library',0);
	 INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES('old-album','library','Existing',0,0),('new-album','library','New',0,0);
	 INSERT INTO album_access(album_id,group_id) VALUES('old-album','group')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var restricted bool
	if err := db.QueryRow(`SELECT access_restricted FROM albums WHERE id='old-album'`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("existing restriction was not migrated", restricted, err)
	}
	if _, err := db.Exec(`INSERT INTO album_access(album_id,group_id) VALUES('new-album','group')`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT access_restricted FROM albums WHERE id='new-album'`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("new restriction was not latched", restricted, err)
	}
	if _, err := db.Exec(`DELETE FROM permission_groups WHERE id='group'`); err == nil {
		t.Fatal("raw group deletion bypassed RESTRICT foreign keys")
	}
	if _, err := db.Exec(`DELETE FROM album_access WHERE group_id='group'; DELETE FROM permission_groups WHERE id='group'`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM albums WHERE access_restricted=1`).Scan(&count); err != nil || count != 2 {
		t.Fatal("deleting the last group widened restrictions", count, err)
	}
}
