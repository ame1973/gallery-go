package store

import (
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeAlbumOrder(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeAlbumSelection(t)
	data, err := fs.ReadFile(migrations.Files, "0021_album_selection.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0021_album_selection.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func TestAlbumOrderMigrationPreservesIndexedTimeMetadataAndHistoricalEvents(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeAlbumOrder(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('l','Library',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('s','l','Source','/never-open-order-upgrade','{}','unavailable',0);
 INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('p','s','Creator','Person',0);
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,display_name,created_at,updated_at,version) VALUES('a','l','s','p','Source title','Override',-1234,99,7);
 INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('m','a','image','Processing.png',0,0);
 UPDATE albums SET cover_media_id='m',cover_override_id='m',access_restricted=1 WHERE id='a';
 INSERT INTO tags(id,name) VALUES('t','Exact tag'); INSERT INTO album_tags VALUES('a','t');
 INSERT INTO change_log(actor_scope,kind,resource_id,operation,version,payload_json,created_at) VALUES('system','album','a','upsert',7,'{"historical_projection":"unchanged"}',99);
 INSERT INTO outbox_events(id,change_seq,created_at) SELECT 'history-outbox',seq,99 FROM change_log WHERE resource_id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var album, tags, history, outbox, ledger int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM albums WHERE id='a' AND source_id='s' AND person_id='p' AND display_name='Override' AND cover_media_id='m' AND cover_override_id='m' AND access_restricted=1 AND created_at=-1234 AND updated_at=99 AND version=7),
 (SELECT COUNT(*) FROM album_tags WHERE album_id='a' AND tag_id='t'),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND version=7 AND payload_json='{"historical_projection":"unchanged"}'),
 (SELECT COUNT(*) FROM outbox_events WHERE id='history-outbox'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0022_album_order.sql')`).Scan(&album, &tags, &history, &outbox, &ledger); err != nil || album != 1 || tags != 1 || history != 1 || outbox != 1 || ledger != 1 {
		t.Fatal("ordering index migration rewrote first-index time/metadata/history", album, tags, history, outbox, ledger, err)
	}
	for name, want := range map[string][]string{
		"idx_album_indexed_page":         {"created_at", "id"},
		"idx_album_library_indexed_page": {"library_id", "created_at", "id"},
		"idx_album_source_indexed_page":  {"source_id", "created_at", "id"},
		"idx_album_person_indexed_page":  {"person_id", "created_at", "id"},
	} {
		rows, err := db.Query(`PRAGMA index_info('` + name + `')`)
		if err != nil {
			t.Fatal(err)
		}
		columns := []string{}
		for rows.Next() {
			var position, cid int
			var column string
			if err := rows.Scan(&position, &cid, &column); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			columns = append(columns, column)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !slices.Equal(columns, want) {
			t.Fatal("chronological index differs from its keyset/filter prefix", name, columns, want, err)
		}
	}
}

func TestAlbumOrderMigrationLateIndexConflictRollsBackEveryEarlierIndex(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumOrder(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX idx_album_source_indexed_page ON tags(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("conflicting chronological source index was ignored")
	}
	var ledger, partial int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0022_album_order.sql'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name IN('idx_album_indexed_page','idx_album_library_indexed_page','idx_album_person_indexed_page'))`).Scan(&ledger, &partial); err != nil || ledger != 0 || partial != 0 {
		t.Fatal("failed ordering DDL left partial indexes or a published ledger", ledger, partial, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_album_source_indexed_page`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("ordering index migration did not retry atomically", err)
	}
}
