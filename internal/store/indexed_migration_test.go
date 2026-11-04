package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func TestIndexedInventoryMigrationPreservesOverridesAndLogicalObjects(t *testing.T) {
	db := testDB(t)
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "0010_" {
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
	 INSERT INTO libraries(id,name,created_at) VALUES('library','Isolated upgrade fixture',0);
	 INSERT INTO sources(id,library_id,name,root_path,mapping_json,created_at) VALUES('source','library','fixture','/never-opened-index-upgrade','{}',0);
	 INSERT INTO people(id,source_name,display_name,created_at) VALUES('person','Alice','Person override',0);
	 INSERT INTO albums(id,library_id,source_id,person_id,source_name,display_name,relative_path,access_restricted,created_at,updated_at) VALUES('album','library','source','person','Source title','Album override','Album',1,0,0);
	 INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,updated_at) VALUES('entry','source','Album/photo.png',1,0,0);
	 INSERT INTO media(id,album_id,source_entry_id,kind,file_name,created_at,updated_at) VALUES('media','album','entry','image','photo.png',0,0);
	 UPDATE albums SET cover_media_id='media' WHERE id='album';
	`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var album, person, cover, override, availability, metadata string
	var restricted bool
	var version int64
	if err := db.QueryRow(`SELECT a.display_name,p.display_name,a.cover_media_id,a.cover_override_id,a.access_restricted,m.availability,m.metadata_json,m.version FROM albums a JOIN people p ON p.id=a.person_id JOIN media m ON m.album_id=a.id`).Scan(&album, &person, &cover, &override, &restricted, &availability, &metadata, &version); err != nil || album != "Album override" || person != "Person override" || cover != "media" || override != "media" || !restricted || availability != "available" || metadata != "{}" || version != 1 {
		t.Fatal("indexed migration rewrote overrides or invented metadata", album, person, cover, override, restricted, availability, metadata, version, err)
	}
	for _, statement := range []string{
		`INSERT INTO media(id,album_id,source_entry_id,kind,file_name,created_at,updated_at) VALUES('duplicate','album','entry','image','duplicate.png',0,0)`,
		`UPDATE media SET availability='invented'`,
		`UPDATE media SET metadata_json='not JSON'`,
		`UPDATE source_entries SET missing_full_scans=-1`,
		`UPDATE albums SET cover_override_id='unknown-media'`,
	} {
		if _, err := db.Exec(statement); err == nil {
			t.Fatal("invalid indexed relationship accepted", statement)
		}
	}
	if _, err := db.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('north','source','North/Alice','Alice',0),('south','source','South/Alice','Alice',0)`); err != nil {
		t.Fatal("equal labels should not collapse identities", err)
	}
	if _, err := db.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('duplicate','source','North/Alice','Other label',0)`); err == nil {
		t.Fatal("duplicate source/path identity accepted")
	}
}
