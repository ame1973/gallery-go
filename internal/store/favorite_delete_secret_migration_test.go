package store

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeFavoriteDeleteConfirmation(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeAlbumPopularityOrder(t)
	data, err := fs.ReadFile(migrations.Files, "0031_album_popularity_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0031_album_popularity_order.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func TestFavoriteDeleteConfirmationMigrationPreservesExistingPrivateState(t *testing.T) {
	db := testDB(t)
	previous := beforeFavoriteDeleteConfirmation(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteFolderSelection(t, db)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var favorites, history, outbox, keyRows, ledger, violations int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM favorites WHERE (user_id='u' AND album_id='a' AND collection_id='b-child' AND version=3) OR (user_id='v' AND album_id='a' AND favorited=1)),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND payload_json='{"historical":"unchanged"}'),
 (SELECT COUNT(*) FROM outbox_events WHERE id='rating-history'),
 (SELECT COUNT(*) FROM favorite_delete_confirmation_secret),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0032_favorite_delete_confirmation_secret.sql'),
 (SELECT COUNT(*) FROM pragma_foreign_key_check)`).Scan(&favorites, &history, &outbox, &keyRows, &ledger, &violations); err != nil || favorites != 2 || history != 1 || outbox != 1 || keyRows != 0 || ledger != 1 || violations != 0 {
		t.Fatal("confirmation migration changed existing data or fabricated a key", favorites, history, outbox, keyRows, ledger, violations, err)
	}
	for _, query := range []string{
		`INSERT INTO favorite_delete_confirmation_secret(id,secret) VALUES(2,randomblob(32))`,
		`INSERT INTO favorite_delete_confirmation_secret(id,secret) VALUES(1,NULL)`,
		`INSERT INTO favorite_delete_confirmation_secret(id,secret) VALUES(1,'01234567890123456789012345678901')`,
		`INSERT INTO favorite_delete_confirmation_secret(id,secret) VALUES(1,randomblob(31))`,
		`INSERT INTO favorite_delete_confirmation_secret(id,secret) VALUES(1,zeroblob(32))`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("confirmation key constraint accepted invalid data", query)
		}
	}
	if _, err := db.Exec(`INSERT INTO favorite_delete_confirmation_secret(id,secret) VALUES(1,randomblob(32))`); err != nil {
		t.Fatal("valid isolated key row was rejected", err)
	}
	if _, err := db.Exec(`INSERT INTO favorite_delete_confirmation_secret(id,secret) VALUES(1,randomblob(32))`); err == nil {
		t.Fatal("installation accepted a second confirmation key")
	}
}

func TestFavoriteDeleteConfirmationMigrationFailureRollsBackAndRetries(t *testing.T) {
	db := testDB(t)
	previous := beforeFavoriteDeleteConfirmation(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteFolderSelection(t, db)
	if _, err := db.Exec(`CREATE TABLE favorite_delete_confirmation_secret(existing TEXT); INSERT INTO favorite_delete_confirmation_secret VALUES('owned fixture')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("conflicting key table was silently overwritten")
	}
	var ledger, favorites, history int
	var existing string
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0032_favorite_delete_confirmation_secret.sql'),
 (SELECT COUNT(*) FROM favorites WHERE user_id='u' AND album_id='a' AND collection_id='b-child' AND version=3),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND payload_json='{"historical":"unchanged"}'),
 (SELECT existing FROM favorite_delete_confirmation_secret)`).Scan(&ledger, &favorites, &history, &existing); err != nil || ledger != 0 || favorites != 1 || history != 1 || existing != "owned fixture" {
		t.Fatal("failed key migration did not preserve old rows and ledger", ledger, favorites, history, existing, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE favorite_delete_confirmation_secret`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("key migration could not retry after fixture repair", err)
	}
}
