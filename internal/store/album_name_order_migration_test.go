package store

import (
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeAlbumNameOrder(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeAlbumOrder(t)
	data, err := fs.ReadFile(migrations.Files, "0022_album_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0022_album_order.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func TestAlbumNameOrderMigrationPreservesMetadataHistoryAndMaintainsExpressionKeys(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeAlbumNameOrder(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('l','Library',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('s','l','Source','/never-open-name-upgrade','{}','unavailable',0);
 INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('p','s','Creator','Person',0);
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,display_name,created_at,updated_at,version) VALUES('a','l','s','p','Zulu','Alpha',-1234,99,7),('b','l','s','p','Beta',NULL,0,0,1);
 INSERT INTO tags(id,name) VALUES('t','Exact tag'); INSERT INTO album_tags VALUES('a','t');
 INSERT INTO change_log(actor_scope,kind,resource_id,operation,version,payload_json,created_at) VALUES('system','album','a','upsert',7,'{"historical_projection":"unchanged"}',99);
 INSERT INTO outbox_events(id,change_seq,created_at) SELECT 'name-history-outbox',seq,99 FROM change_log WHERE resource_id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var preserved, tags, history, outbox, ledger, indexes int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM albums WHERE id='a' AND source_id='s' AND person_id='p' AND display_name='Alpha' AND source_name='Zulu' AND created_at=-1234 AND updated_at=99 AND version=7),
 (SELECT COUNT(*) FROM album_tags WHERE album_id='a' AND tag_id='t'),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND version=7 AND payload_json='{"historical_projection":"unchanged"}'),
 (SELECT COUNT(*) FROM outbox_events WHERE id='name-history-outbox'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0023_album_name_order.sql'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name IN('idx_album_title_page','idx_album_library_title_page','idx_album_source_title_page','idx_album_person_title_page'))`).Scan(&preserved, &tags, &history, &outbox, &ledger, &indexes); err != nil || preserved != 1 || tags != 1 || history != 1 || outbox != 1 || ledger != 1 || indexes != 4 {
		t.Fatal("name expression migration rewrote data/history or missed an index", preserved, tags, history, outbox, ledger, indexes, err)
	}
	ordered := func(want ...string) {
		t.Helper()
		rows, err := db.Query(`SELECT id FROM albums INDEXED BY idx_album_title_page ORDER BY COALESCE(NULLIF(display_name,''),source_name) COLLATE BINARY,id`)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !slices.Equal(ids, want) {
			t.Fatal("expression index lost literal override/fallback order", ids, want, err)
		}
	}
	ordered("a", "b")
	// Isolated direct SQL verifies SQLite expression-key maintenance only; actual
	// authorized override/scan transactions are covered by native API regressions.
	if _, err := db.Exec(`UPDATE albums SET display_name='' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	ordered("b", "a")
	if _, err := db.Exec(`UPDATE albums SET source_name='A-source' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	ordered("a", "b")
	var collation string
	if err := db.QueryRow(`SELECT coll FROM pragma_index_xinfo('idx_album_title_page') WHERE cid=-2 AND "key"=1`).Scan(&collation); err != nil || collation != "BINARY" {
		t.Fatal("title expression index is not literal BINARY collation", collation, err)
	}
}

func TestAlbumNameOrderMigrationLateConflictRollsBackAllEarlierIndexes(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumNameOrder(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE INDEX idx_album_source_title_page ON tags(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("conflicting title source index was ignored")
	}
	var ledger, partial int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0023_album_name_order.sql'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name IN('idx_album_title_page','idx_album_library_title_page','idx_album_person_title_page'))`).Scan(&ledger, &partial); err != nil || ledger != 0 || partial != 0 {
		t.Fatal("failed title index migration left partial schema/history", ledger, partial, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_album_source_title_page`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("name index migration did not retry atomically", err)
	}
}
