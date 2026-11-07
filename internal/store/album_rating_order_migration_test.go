package store

import (
	"database/sql"
	"io/fs"
	"math"
	"reflect"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeAlbumRatingOrder(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeAlbumRatings(t)
	data, err := fs.ReadFile(migrations.Files, "0024_album_ratings.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0024_album_ratings.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func assertNativeAlbumMean(t *testing.T, db *sql.DB, id string) *float64 {
	t.Helper()
	var cached, native *float64
	var kind string
	if err := db.QueryRow(`SELECT a.rating_avg,typeof(a.rating_avg),
 (SELECT AVG(r.rating) FROM ratings r WHERE r.album_id=a.id) FROM albums a WHERE a.id=?`, id).Scan(&cached, &kind, &native); err != nil || !reflect.DeepEqual(cached, native) || cached != nil && kind != "real" || cached == nil && kind != "null" {
		t.Fatal("maintained mean differs from native AVG/type", id, cached, native, kind, err)
	}
	return cached
}

func TestAlbumRatingOrderMigrationBackfillsNativeMeansWithoutRewritingHistory(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeAlbumRatingOrder(t)); err != nil {
		t.Fatal(err)
	}
	seedRatingUpgrade(t, db)
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,scope_id,created_at,updated_at) VALUES
 ('v','second-voter','not-a-login-hash','user','second-scope',0,0),('w','third-voter','not-a-login-hash','user','third-scope',0,0);
 INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES('v','a',2,456),('w','a',1,789);
 UPDATE ratings SET rating=NULL,version=2 WHERE user_id='u' AND album_id='b'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	mean := assertNativeAlbumMean(t, db, "a")
	if mean == nil || math.Float64bits(*mean) != math.Float64bits(8.0/3) || assertNativeAlbumMean(t, db, "b") != nil {
		t.Fatal("native fractional/null means were rounded, guessed or counted as zero", mean)
	}
	var metadata, history, outbox, votes, indexes, triggers, ledger, fk int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM albums WHERE id='a' AND source_id='s' AND source_name='Indexed' AND display_name='Override' AND version=7 AND created_at=-1234 AND updated_at=99),
 (SELECT COUNT(*) FROM change_log WHERE payload_json='{"historical":"unchanged"}' AND version=7),
 (SELECT COUNT(*) FROM outbox_events WHERE id='rating-history'),
 (SELECT COUNT(*) FROM ratings WHERE (user_id='u' AND album_id='a' AND rating=5 AND updated_at=-1234 AND version=1) OR (user_id='u' AND album_id='b' AND rating IS NULL AND updated_at=5678 AND version=2)),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name IN('idx_album_rating_page','idx_album_library_rating_page','idx_album_source_rating_page','idx_album_person_rating_page')),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND name IN('tr_album_rating_insert','tr_album_rating_update','tr_album_rating_delete')),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0025_album_rating_order.sql'),
 (SELECT COUNT(*) FROM pragma_foreign_key_check)`).Scan(&metadata, &history, &outbox, &votes, &indexes, &triggers, &ledger, &fk); err != nil || metadata != 1 || history != 1 || outbox != 1 || votes != 2 || indexes != 4 || triggers != 3 || ledger != 1 || fk != 0 {
		t.Fatal("rank migration changed identities/versions/votes/history or missed schema/FKs", metadata, history, outbox, votes, indexes, triggers, ledger, fk, err)
	}
	for _, invalid := range []any{0, 6, "not-a-mean", []byte{5}, math.Inf(1), math.Inf(-1)} {
		if _, err := db.Exec(`UPDATE albums SET rating_avg=? WHERE id='a'`, invalid); err == nil {
			t.Fatal("invalid stored score passed the native mean constraint", invalid)
		}
	}
	assertNativeAlbumMean(t, db, "a")
}

func TestAlbumRatingOrderTriggersCoverClearMoveReplaceCascadeAndRollback(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	seedRatingUpgrade(t, db)
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,scope_id,created_at,updated_at) VALUES
 ('v','other-voter','not-a-login-hash','user','other-scope',0,0);
 INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES('v','a',1,0)`); err != nil {
		t.Fatal(err)
	}
	if mean := assertNativeAlbumMean(t, db, "a"); mean == nil || *mean != 3 {
		t.Fatal("insert trigger omitted a live vote", mean)
	}
	for _, query := range []string{
		`UPDATE ratings SET rating=NULL WHERE user_id='u' AND album_id='a'`,
		`UPDATE ratings SET rating=5 WHERE user_id='u' AND album_id='a'`,
		// INSERT OR REPLACE can suppress implicit delete triggers. The INSERT
		// trigger must still recompute when replacing a live vote with null.
		`INSERT OR REPLACE INTO ratings(user_id,album_id,rating,updated_at,version) VALUES('u','a',NULL,77,9)`,
		`UPDATE ratings SET album_id='b' WHERE user_id='v' AND album_id='a'`,
		`UPDATE ratings SET rating=4 WHERE user_id='u' AND album_id='b'`,
		`DELETE FROM users WHERE id='u'`,
		`DELETE FROM ratings WHERE user_id='v'`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(query, err)
		}
		assertNativeAlbumMean(t, db, "a")
		assertNativeAlbumMean(t, db, "b")
	}
	if assertNativeAlbumMean(t, db, "a") != nil || assertNativeAlbumMean(t, db, "b") != nil {
		t.Fatal("zero votes fabricated a real zero score")
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES('v','a',5,0)`); err != nil {
		t.Fatal(err)
	}
	var staged float64
	if err := tx.QueryRow(`SELECT rating_avg FROM albums WHERE id='a'`).Scan(&staged); err != nil || staged != 5 {
		t.Fatal("trigger was not part of the actual writer transaction", staged, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if assertNativeAlbumMean(t, db, "a") != nil {
		t.Fatal("rollback retained a derived score without its vote")
	}
	// Derived-field maintenance does not impersonate the authorized version/event
	// producer. Manual API and auth deletion tests assert those co-committed writes.
	var version, changes int
	if err := db.QueryRow(`SELECT version,(SELECT COUNT(*) FROM change_log) FROM albums WHERE id='a'`).Scan(&version, &changes); err != nil || version != 7 || changes != 1 {
		t.Fatal("cache triggers invented business versions/events", version, changes, err)
	}
}

func TestAlbumRatingOrderMigrationLateConflictRollsBackBackfillTriggersAndIndexes(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumRatingOrder(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedRatingUpgrade(t, db)
	if _, err := db.Exec(`CREATE INDEX idx_album_person_rating_page ON tags(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("late rank index conflict was silently ignored")
	}
	var column, triggers, indexes, ledger, votes int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM pragma_table_info('albums') WHERE name='rating_avg'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name IN('tr_album_rating_insert','tr_album_rating_update','tr_album_rating_delete')),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name IN('idx_album_rating_page','idx_album_library_rating_page','idx_album_source_rating_page')),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0025_album_rating_order.sql'),
 (SELECT COUNT(*) FROM ratings WHERE (album_id='a' AND rating=5 AND updated_at=-1234) OR (album_id='b' AND rating=2 AND updated_at=5678))`).Scan(&column, &triggers, &indexes, &ledger, &votes); err != nil || column != 0 || triggers != 0 || indexes != 0 || ledger != 0 || votes != 2 {
		t.Fatal("failed rank migration retained partial projection or changed votes", column, triggers, indexes, ledger, votes, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_album_person_rating_page`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("rank migration could not be retried after removing its own fixture collision", err)
	}
	assertNativeAlbumMean(t, db, "a")
	assertNativeAlbumMean(t, db, "b")
}
