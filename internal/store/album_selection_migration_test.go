package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeAlbumSelection(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeMediaSelection(t)
	data, err := fs.ReadFile(migrations.Files, "0020_media_selection.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0020_media_selection.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func TestAlbumSelectionMigrationPreservesSourcePersonTagsCoversAndHistory(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeAlbumSelection(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('l','Library',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('s','l','Source','/never-open-selection-upgrade','{}','unavailable',0);
 INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('p','s','Creator','Person',0);
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,display_name,created_at,updated_at) VALUES('a','l','s','p','Source title','Display override',0,0);
 INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('m','a','image','Processing.png',0,0);
 UPDATE albums SET cover_media_id='m',cover_override_id='m',access_restricted=1 WHERE id='a';
 INSERT INTO tags(id,name) VALUES('t','Exact tag'); INSERT INTO album_tags VALUES('a','t');
 INSERT INTO jobs(id,kind,payload_json,status,next_run_at,created_at,updated_at) VALUES('history','scan','{"version":1}','cancelled',0,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var album, tags, history, index, ledger int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM albums WHERE id='a' AND source_id='s' AND person_id='p' AND display_name='Display override' AND cover_media_id='m' AND cover_override_id='m' AND access_restricted=1),
 (SELECT COUNT(*) FROM album_tags WHERE album_id='a' AND tag_id='t'),
 (SELECT COUNT(*) FROM jobs WHERE id='history' AND status='cancelled'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='idx_album_source_page'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0021_album_selection.sql')`).Scan(&album, &tags, &history, &index, &ledger); err != nil || album != 1 || tags != 1 || history != 1 || index != 1 || ledger != 1 {
		t.Fatal("album source ordering changed metadata/history or left an incomplete migration", album, tags, history, index, ledger, err)
	}
	rows, err := db.Query(`PRAGMA index_info('idx_album_source_page')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := []string{}
	for rows.Next() {
		var position, cid int
		var name string
		if err := rows.Scan(&position, &cid, &name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil || len(columns) != 2 || columns[0] != "source_id" || columns[1] != "id" {
		t.Fatal("source keyset index does not match its selection order", columns, err)
	}
}

func TestAlbumSelectionMigrationConflictingIndexNeverPublishesItsLedger(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumSelection(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX idx_album_source_page ON tags(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("conflicting source index was ignored")
	}
	var ledger int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version='0021_album_selection.sql'`).Scan(&ledger); err != nil || ledger != 0 {
		t.Fatal("failed album index migration published a ledger row", ledger, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_album_source_page`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("album source index migration could not retry cleanly", err)
	}
}
