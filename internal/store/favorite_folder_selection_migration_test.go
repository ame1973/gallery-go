package store

import (
	"database/sql"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeFavoriteFolderSelection(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeFavoriteCollectionTree(t)
	data, err := fs.ReadFile(migrations.Files, "0028_favorite_collection_tree.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0028_favorite_collection_tree.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func seedFavoriteFolderSelection(t *testing.T, db *sql.DB) {
	t.Helper()
	seedFavoriteCollectionTree(t, db)
	if _, err := db.Exec(`UPDATE favorite_collections SET updated_at=created_at;
 INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES('c','l','s','Third',0,0),('d','l','s','Fourth',0,0);
 INSERT INTO favorites(user_id,album_id,collection_id,created_at,favorited,updated_at,version) VALUES('u','c',NULL,0,1,100,2),('u','d',NULL,0,0,200,5)`); err != nil {
		t.Fatal(err)
	}
}

func TestFavoriteFolderSelectionMigrationPreservesLiveAndClearedRows(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeFavoriteFolderSelection(t)); err != nil {
		t.Fatal(err)
	}
	seedFavoriteFolderSelection(t, db)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var favorites, folders, history, outbox, index, ledger, violations, fk int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM favorites WHERE (user_id='u' AND album_id='a' AND collection_id='b-child' AND favorited=1 AND version=3) OR (user_id='u' AND album_id='b' AND collection_id IS NULL AND favorited=0 AND version=4) OR (user_id='u' AND album_id='c' AND collection_id IS NULL AND favorited=1 AND updated_at=100 AND version=2) OR (user_id='u' AND album_id='d' AND collection_id IS NULL AND favorited=0 AND updated_at=200 AND version=5) OR (user_id='v' AND album_id='a' AND favorited=1 AND version=2)),
 (SELECT COUNT(*) FROM favorite_collections WHERE id IN ('a-root','b-child','c-grand','d-sibling','v-root','selection-folder') AND version=1 AND updated_at=created_at),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND payload_json='{"historical":"unchanged"}'),
 (SELECT COUNT(*) FROM outbox_events WHERE id='rating-history'),
 (SELECT COUNT(*) FROM pragma_index_list('favorites') WHERE name='idx_favorites_live_collection_page' AND partial=1),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0029_favorite_folder_selection.sql'),
 (SELECT COUNT(*) FROM pragma_foreign_key_check),(SELECT foreign_keys FROM pragma_foreign_keys)`).Scan(&favorites, &folders, &history, &outbox, &index, &ledger, &violations, &fk); err != nil || favorites != 5 || folders != 6 || history != 1 || outbox != 1 || index != 1 || ledger != 1 || violations != 0 || fk != 1 {
		t.Fatal("folder index migration changed private data/history/FKs or missed its partial index", favorites, folders, history, outbox, index, ledger, violations, fk, err)
	}
	live := func(queryer interface {
		Query(string, ...any) (*sql.Rows, error)
	}, collection any) []string {
		t.Helper()
		rows, err := queryer.Query(`SELECT album_id FROM favorites INDEXED BY idx_favorites_live_collection_page WHERE user_id=? AND collection_id IS ? AND favorited=1 ORDER BY album_id`, "u", collection)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return ids
	}
	if ids := live(db, "b-child"); !slices.Equal(ids, []string{"a"}) {
		t.Fatal("folder index included another collection/owner", ids)
	}
	if ids := live(db, nil); !slices.Equal(ids, []string{"c"}) {
		t.Fatal("default index included cleared revisions or another account", ids)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE favorites SET collection_id=NULL WHERE user_id='u' AND album_id='a';
 UPDATE favorites SET favorited=0 WHERE user_id='u' AND album_id='c'`); err != nil {
		t.Fatal(err)
	}
	if ids := live(tx, nil); !slices.Equal(ids, []string{"a"}) {
		t.Fatal("folder index did not track move and clear", ids)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if ids := live(db, nil); !slices.Equal(ids, []string{"c"}) {
		t.Fatal("rolled-back folder changes escaped the index", ids)
	}
}

func TestFavoriteFolderSelectionMigrationCollisionRollsBackAndRetries(t *testing.T) {
	db := testDB(t)
	previous := beforeFavoriteFolderSelection(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteFolderSelection(t, db)
	if _, err := db.Exec(`CREATE INDEX idx_favorites_live_collection_page ON tags(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("folder selection index collision was ignored")
	}
	var retained, ledger int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorites WHERE (user_id='u' AND album_id='a' AND collection_id='b-child' AND favorited=1 AND version=3) OR (user_id='u' AND album_id='b' AND favorited=0 AND version=4) OR (user_id='u' AND album_id='c' AND favorited=1 AND version=2) OR (user_id='u' AND album_id='d' AND favorited=0 AND version=5) OR (user_id='v' AND album_id='a' AND favorited=1 AND version=2)),(SELECT COUNT(*) FROM schema_migrations WHERE version='0029_favorite_folder_selection.sql')`).Scan(&retained, &ledger); err != nil || retained != 5 || ledger != 0 {
		t.Fatal("failed folder index migration changed data or ledger", retained, ledger, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_favorites_live_collection_page`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("folder index migration could not retry after removing the fixture collision", err)
	}
}
