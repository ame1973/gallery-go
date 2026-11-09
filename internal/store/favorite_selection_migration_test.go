package store

import (
	"database/sql"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeFavoriteSelection(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeAlbumFavorites(t)
	data, err := fs.ReadFile(migrations.Files, "0026_album_favorites.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0026_album_favorites.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func seedFavoriteSelectionUpgrade(t *testing.T, db *sql.DB) {
	t.Helper()
	seedRatingUpgrade(t, db)
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,scope_id,created_at,updated_at) VALUES('v','selection-other','not-a-login-hash','user','other-scope',0,0);
 INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES('selection-folder','u','One',0);
 INSERT INTO favorites(user_id,album_id,collection_id,created_at,favorited,updated_at,version) VALUES
 ('u','a','selection-folder',-1234,1,456,3),('u','b',NULL,0,0,789,4),('v','a',NULL,0,1,0,2)`); err != nil {
		t.Fatal(err)
	}
}

func TestFavoriteSelectionMigrationPreservesStateAndMaintainsPartialIndex(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeFavoriteSelection(t)); err != nil {
		t.Fatal(err)
	}
	seedFavoriteSelectionUpgrade(t, db)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var favorites, album, history, outbox, index, ledger, violations, fk int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM favorites WHERE (user_id='u' AND album_id='a' AND collection_id='selection-folder' AND created_at=-1234 AND favorited=1 AND updated_at=456 AND version=3) OR (user_id='u' AND album_id='b' AND collection_id IS NULL AND favorited=0 AND updated_at=789 AND version=4) OR (user_id='v' AND album_id='a' AND favorited=1 AND version=2)),
 (SELECT COUNT(*) FROM albums WHERE id='a' AND display_name='Override' AND version=7 AND created_at=-1234 AND updated_at=99),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND payload_json='{"historical":"unchanged"}'),
 (SELECT COUNT(*) FROM outbox_events WHERE id='rating-history'),
 (SELECT COUNT(*) FROM pragma_index_list('favorites') WHERE name='idx_favorites_active_page' AND partial=1),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0027_favorite_selection.sql'),
 (SELECT COUNT(*) FROM pragma_foreign_key_check),(SELECT foreign_keys FROM pragma_foreign_keys)`).Scan(&favorites, &album, &history, &outbox, &index, &ledger, &violations, &fk); err != nil || favorites != 3 || album != 1 || history != 1 || outbox != 1 || index != 1 || ledger != 1 || violations != 0 || fk != 1 {
		t.Fatal("favorite selection migration changed state/history/FKs or missed its partial index", favorites, album, history, outbox, index, ledger, violations, fk, err)
	}
	live := func(queryer interface {
		Query(string, ...any) (*sql.Rows, error)
	}) []string {
		t.Helper()
		rows, err := queryer.Query(`SELECT album_id FROM favorites INDEXED BY idx_favorites_active_page WHERE user_id=? AND favorited=1 ORDER BY album_id`, "u")
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
	if ids := live(db); !slices.Equal(ids, []string{"a"}) {
		t.Fatal("partial index included a tombstone or another account", ids)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE favorites SET favorited=0,collection_id=NULL WHERE user_id='u' AND album_id='a';
 UPDATE favorites SET favorited=1 WHERE user_id='u' AND album_id='b'`); err != nil {
		t.Fatal(err)
	}
	if ids := live(tx); !slices.Equal(ids, []string{"b"}) {
		t.Fatal("partial index did not follow real state changes", ids)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if ids := live(db); !slices.Equal(ids, []string{"a"}) {
		t.Fatal("rolled-back favorite changes escaped the partial index", ids)
	}
}

func TestFavoriteSelectionMigrationIndexCollisionRollsBackAndRetries(t *testing.T) {
	db := testDB(t)
	previous := beforeFavoriteSelection(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteSelectionUpgrade(t, db)
	if _, err := db.Exec(`CREATE INDEX idx_favorites_active_page ON tags(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("favorite selection index collision was ignored")
	}
	var retained, ledger int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorites WHERE (user_id='u' AND album_id='a' AND favorited=1 AND version=3) OR (user_id='u' AND album_id='b' AND favorited=0 AND version=4) OR (user_id='v' AND album_id='a' AND favorited=1 AND version=2)),(SELECT COUNT(*) FROM schema_migrations WHERE version='0027_favorite_selection.sql')`).Scan(&retained, &ledger); err != nil || retained != 3 || ledger != 0 {
		t.Fatal("failed index migration changed personal state or its ledger", retained, ledger, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_favorites_active_page`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("favorite selection migration could not retry after the fixture collision was removed", err)
	}
}
