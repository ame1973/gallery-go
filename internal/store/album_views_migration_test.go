package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeAlbumViews(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeFavoriteFolderSelection(t)
	data, err := fs.ReadFile(migrations.Files, "0029_favorite_folder_selection.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0029_favorite_folder_selection.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func TestAlbumViewsMigrationPreservesRowsAndMaintainsTotals(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeAlbumViews(t)); err != nil {
		t.Fatal(err)
	}
	seedFavoriteFolderSelection(t, db)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var albums, favorites, history, ledger, violations, fk int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM albums WHERE id IN ('a','b','c','d') AND view_count=0),
 (SELECT COUNT(*) FROM favorites WHERE (user_id='u' AND album_id='a' AND collection_id='b-child' AND version=3) OR (user_id='v' AND album_id='a' AND favorited=1)),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND payload_json='{"historical":"unchanged"}'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0030_album_views.sql'),
 (SELECT COUNT(*) FROM pragma_foreign_key_check),(SELECT foreign_keys FROM pragma_foreign_keys)`).Scan(&albums, &favorites, &history, &ledger, &violations, &fk); err != nil || albums != 4 || favorites != 2 || history != 1 || ledger != 1 || violations != 0 || fk != 1 {
		t.Fatal("view migration changed indexed content, personal rows, history or FKs", albums, favorites, history, ledger, violations, fk, err)
	}
	if _, err := db.Exec(`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version) VALUES('u','a',2,100,3),('v','a',3,101,4),('u','c',1,102,2)`); err != nil {
		t.Fatal(err)
	}
	var total int64
	if err := db.QueryRow(`SELECT view_count FROM albums WHERE id='a'`).Scan(&total); err != nil || total != 5 {
		t.Fatal("view insertion did not maintain the shared total", total, err)
	}
	for _, invalid := range []string{
		`INSERT OR REPLACE INTO album_views(user_id,album_id,view_count,last_viewed_at,version) VALUES('u','a',99,103,5)`,
		`UPDATE album_views SET album_id='b' WHERE user_id='u' AND album_id='a'`,
		`UPDATE album_views SET user_id='v' WHERE user_id='u' AND album_id='a'`,
		`UPDATE album_views SET view_count=0 WHERE user_id='u' AND album_id='a'`,
		`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version) VALUES('missing','a',1,0,2)`,
	} {
		if _, err := db.Exec(invalid); err == nil {
			t.Fatal("invalid identity, replacement or counter was accepted", invalid)
		}
	}
	if err := db.QueryRow(`SELECT view_count FROM albums WHERE id='a'`).Scan(&total); err != nil || total != 5 {
		t.Fatal("rejected writes changed the shared total", total, err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE album_views SET view_count=4 WHERE user_id='u' AND album_id='a'; DELETE FROM album_views WHERE user_id='v' AND album_id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(`SELECT view_count FROM albums WHERE id='a'`).Scan(&total); err != nil || total != 4 {
		t.Fatal("transactional update/delete lost the derived total", total, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT view_count FROM albums WHERE id='a'`).Scan(&total); err != nil || total != 5 {
		t.Fatal("rolled-back view writes escaped the transaction", total, err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id='u'`); err != nil {
		t.Fatal("account cascade did not preserve other users' data", err)
	}
	var other, empty, folders int
	if err := db.QueryRow(`SELECT (SELECT view_count FROM albums WHERE id='a'),(SELECT view_count FROM albums WHERE id='c'),(SELECT COUNT(*) FROM favorite_collections WHERE user_id='u')`).Scan(&other, &empty, &folders); err != nil || other != 3 || empty != 0 || folders != 0 {
		t.Fatal("account cascade left stale totals or lost another owner's views", other, empty, folders, err)
	}
}

func TestAlbumViewsMigrationCollisionRollsBackAndRetries(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumViews(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteFolderSelection(t, db)
	if _, err := db.Exec(`CREATE TABLE album_views(collision INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("view table collision was ignored")
	}
	var column, ledger, preserved int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM pragma_table_info('albums') WHERE name='view_count'),(SELECT COUNT(*) FROM schema_migrations WHERE version='0030_album_views.sql'),(SELECT COUNT(*) FROM favorites WHERE user_id='u' AND album_id='a' AND collection_id='b-child' AND version=3)`).Scan(&column, &ledger, &preserved); err != nil || column != 0 || ledger != 0 || preserved != 1 {
		t.Fatal("failed view migration left a partial column, ledger or data loss", column, ledger, preserved, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE album_views`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("view migration could not retry after removing the fixture collision", err)
	}
}
