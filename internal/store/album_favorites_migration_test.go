package store

import (
	"database/sql"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeAlbumFavorites(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeAlbumRatingOrder(t)
	data, err := fs.ReadFile(migrations.Files, "0025_album_rating_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0025_album_rating_order.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func seedFavoriteUpgrade(t *testing.T, db *sql.DB) {
	t.Helper()
	seedRatingUpgrade(t, db)
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,scope_id,created_at,updated_at) VALUES('v','other-favorite-owner','not-a-login-hash','user','other-scope',0,0);
 INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('parent','u',NULL,'Parent',-1234),('child','u','parent','Child',0),('other','v',NULL,'Other',0);
 INSERT INTO favorites(user_id,album_id,collection_id,created_at) VALUES('u','a','child',-1234),('u','b',NULL,0),('v','a','other',5678)`); err != nil {
		t.Fatal(err)
	}
}

func TestAlbumFavoritesMigrationPreservesMembershipsHistoryAndAccountCascades(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeAlbumFavorites(t)); err != nil {
		t.Fatal(err)
	}
	seedFavoriteUpgrade(t, db)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var memberships, preserved, history, outbox, ledger, indexes, violations, fk int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM favorites WHERE favorited=1 AND version=1 AND updated_at=created_at AND ((user_id='u' AND album_id='a' AND collection_id='child' AND created_at=-1234) OR (user_id='u' AND album_id='b' AND collection_id IS NULL AND created_at=0) OR (user_id='v' AND album_id='a' AND collection_id='other' AND created_at=5678))),
 (SELECT COUNT(*) FROM albums WHERE id='a' AND source_id='s' AND display_name='Override' AND version=7 AND created_at=-1234 AND updated_at=99 AND rating_avg=5),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND payload_json='{"historical":"unchanged"}'),
 (SELECT COUNT(*) FROM outbox_events WHERE id='rating-history'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0026_album_favorites.sql'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name IN ('uq_collection_owner_id','idx_favorites_collection')),
 (SELECT COUNT(*) FROM pragma_foreign_key_check),(SELECT foreign_keys FROM pragma_foreign_keys)`).Scan(&memberships, &preserved, &history, &outbox, &ledger, &indexes, &violations, &fk); err != nil || memberships != 3 || preserved != 1 || history != 1 || outbox != 1 || ledger != 1 || indexes != 2 || violations != 0 || fk != 1 {
		t.Fatal("favorite upgrade lost state/history/constraints", memberships, preserved, history, outbox, ledger, indexes, violations, fk, err)
	}
	for _, query := range []string{
		`UPDATE favorites SET collection_id='other' WHERE user_id='u' AND album_id='a'`,
		`UPDATE favorites SET collection_id='missing' WHERE user_id='u' AND album_id='a'`,
		`UPDATE favorites SET favorited=0 WHERE user_id='u' AND album_id='a'`,
		`UPDATE favorites SET user_id='missing' WHERE user_id='u' AND album_id='b'`,
		`UPDATE favorites SET album_id='missing' WHERE user_id='u' AND album_id='b'`,
		`DELETE FROM favorite_collections WHERE id='child'`,
		`UPDATE favorite_collections SET user_id='v' WHERE id='child'`,
		`INSERT INTO favorites(user_id,album_id,created_at) VALUES('u','a',0)`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("ownership/identity/explicit collection-deletion constraint was bypassed", query)
		}
	}
	for _, column := range []string{"favorited", "version"} {
		values := []any{-1, 1.5, []byte{1}}
		if column == "favorited" {
			values = append(values, 2)
		} else {
			values = append(values, 0)
		}
		for _, value := range values {
			if _, err := db.Exec(`UPDATE favorites SET `+column+`=? WHERE user_id='u' AND album_id='b'`, value); err == nil {
				t.Fatal("invalid favorite type/value accepted", column, value)
			}
		}
	}
	for _, query := range []string{
		`SELECT favorited,collection_id,updated_at,version FROM favorites WHERE user_id='u' AND album_id='a'`,
		`SELECT rowid FROM favorites WHERE user_id='u' AND collection_id='child'`,
		`SELECT id FROM favorite_collections WHERE user_id='u' AND id='child'`,
	} {
		rows, err := db.Query(`EXPLAIN QUERY PLAN ` + query)
		if err != nil {
			t.Fatal(err)
		}
		plan := ""
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan += detail
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !strings.Contains(plan, "SEARCH") || !strings.Contains(plan, "INDEX") || strings.Contains(plan, "SCAN") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatal("favorite point/FK ownership check is not indexed", query, plan, err)
		}
	}
	if _, err := db.Exec(`UPDATE favorites SET favorited=0,collection_id=NULL,version=2 WHERE user_id='u' AND album_id='b'`); err != nil {
		t.Fatal("valid cleared tombstone rejected", err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id='u'`); err != nil {
		t.Fatal("single-statement account cascade could not remove its nested collections and live/cleared favorites", err)
	}
	var own, other, collections, albums int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorites WHERE user_id='u'),(SELECT COUNT(*) FROM favorites WHERE user_id='v' AND album_id='a' AND collection_id='other' AND favorited=1 AND version=1),(SELECT COUNT(*) FROM favorite_collections),(SELECT COUNT(*) FROM albums)`).Scan(&own, &other, &collections, &albums); err != nil || own != 0 || other != 1 || collections != 1 || albums != 2 {
		t.Fatal("account cascade retained private data or removed another account/gallery", own, other, collections, albums, err)
	}
	if _, err := db.Exec(`DELETE FROM albums WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM favorites`).Scan(&own); err != nil || own != 0 {
		t.Fatal("album FK cascade retained a membership", own, err)
	}
}

func TestAlbumFavoritesMigrationLateIndexFailureRestoresOldMembershipsAndLedger(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumFavorites(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteUpgrade(t, db)
	if _, err := db.Exec(`CREATE INDEX idx_favorites_collection ON tags(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("late favorite-index collision ignored")
	}
	var columns, partial, ledger, retained int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM pragma_table_info('favorites') WHERE name IN ('favorited','updated_at','version')),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name IN ('favorites_next','uq_collection_owner_id')),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0026_album_favorites.sql'),
 (SELECT COUNT(*) FROM favorites WHERE (user_id='u' AND album_id='a' AND collection_id='child' AND created_at=-1234) OR (user_id='u' AND album_id='b' AND collection_id IS NULL AND created_at=0) OR (user_id='v' AND album_id='a' AND collection_id='other' AND created_at=5678))`).Scan(&columns, &partial, &ledger, &retained); err != nil || columns != 0 || partial != 0 || ledger != 0 || retained != 3 {
		t.Fatal("failed favorite rebuild retained partial schema or lost old memberships", columns, partial, ledger, retained, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_favorites_collection`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("favorite migration could not retry after removing only the fixture collision", err)
	}
}

func TestAlbumFavoritesMigrationRejectsCrossAccountAssignmentsWithoutRepairOrDataLoss(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumFavorites(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteUpgrade(t, db)
	if _, err := db.Exec(`UPDATE favorites SET collection_id='other' WHERE user_id='u' AND album_id='a'`); err != nil {
		t.Fatal("the old global FK fixture did not permit the invalid ownership", err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("cross-account legacy assignment silently accepted")
	}
	var retained, partial int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorites WHERE user_id='u' AND album_id='a' AND collection_id='other' AND created_at=-1234),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name IN ('favorites_next','uq_collection_owner_id','idx_favorites_collection'))`).Scan(&retained, &partial); err != nil || retained != 1 || partial != 0 {
		t.Fatal("strict ownership migration repaired/deleted private data or left partial schema", retained, partial, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE favorites SET collection_id='child' WHERE user_id='u' AND album_id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
}
