package store

import (
	"database/sql"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeAlbumRatings(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeAlbumNameOrder(t)
	data, err := fs.ReadFile(migrations.Files, "0023_album_name_order.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0023_album_name_order.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func seedRatingUpgrade(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,scope_id,created_at,updated_at) VALUES('u','upgrade-voter','not-a-login-hash','user','upgrade-scope',0,0);
 INSERT INTO libraries(id,name,created_at) VALUES('l','Retained',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('s','l','Source','/never-open-rating-upgrade','{}','unavailable',0);
 INSERT INTO albums(id,library_id,source_id,source_name,display_name,version,created_at,updated_at) VALUES('a','l','s','Indexed','Override',7,-1234,99),('b','l','s','Other',NULL,8,0,0);
 INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES('u','a',5,-1234),('u','b',2,5678);
 INSERT INTO change_log(actor_scope,kind,resource_id,operation,version,payload_json,created_at) VALUES('system','album','a','upsert',7,'{"historical":"unchanged"}',99);
 INSERT INTO outbox_events(id,change_seq,created_at) SELECT 'rating-history',seq,99 FROM change_log WHERE resource_id='a'`); err != nil {
		t.Fatal(err)
	}
}

func TestAlbumRatingsMigrationPreservesVotesMetadataHistoryAndCompositeKeys(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeAlbumRatings(t)); err != nil {
		t.Fatal(err)
	}
	seedRatingUpgrade(t, db)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var votes, album, history, outbox, indexes, ledger, violations, fk int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM ratings WHERE user_id='u' AND version=1 AND ((album_id='a' AND rating=5 AND updated_at=-1234) OR (album_id='b' AND rating=2 AND updated_at=5678))),
 (SELECT COUNT(*) FROM albums WHERE id='a' AND source_id='s' AND source_name='Indexed' AND display_name='Override' AND version=7 AND created_at=-1234 AND updated_at=99),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND version=7 AND payload_json='{"historical":"unchanged"}'),
 (SELECT COUNT(*) FROM outbox_events WHERE id='rating-history'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND tbl_name='ratings' AND name='idx_ratings_album'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0024_album_ratings.sql'),
 (SELECT COUNT(*) FROM pragma_foreign_key_check),(SELECT foreign_keys FROM pragma_foreign_keys)`).Scan(&votes, &album, &history, &outbox, &indexes, &ledger, &violations, &fk); err != nil || votes != 2 || album != 1 || history != 1 || outbox != 1 || indexes != 1 || ledger != 1 || violations != 0 || fk != 1 {
		t.Fatal("rating migration rewrote votes/metadata/history, duplicated index or weakened FKs", votes, album, history, outbox, indexes, ledger, violations, fk, err)
	}
	for _, value := range []any{0, 6, 1.5, []byte{5}} {
		if _, err := db.Exec(`UPDATE ratings SET rating=? WHERE user_id='u' AND album_id='a'`, value); err == nil {
			t.Fatal("non-integer or out-of-range vote bypassed the new constraint", value)
		}
	}
	for _, value := range []any{0, -1, 1.5} {
		if _, err := db.Exec(`UPDATE ratings SET version=? WHERE user_id='u' AND album_id='a'`, value); err == nil {
			t.Fatal("non-positive or fractional private revision was accepted", value)
		}
	}
	if _, err := db.Exec(`UPDATE ratings SET rating=NULL,version=2 WHERE user_id='u' AND album_id='a'`); err != nil {
		t.Fatal("null tombstone was rejected", err)
	}
	if _, err := db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES('u','a',4,0)`); err == nil {
		t.Fatal("second vote for the same account/album was accepted")
	}
	if _, err := db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES('missing','a',4,0)`); err == nil {
		t.Fatal("rating lost its enforced user FK")
	}
	for _, query := range []string{
		`SELECT AVG(rating) FROM ratings WHERE album_id='a'`,
		`SELECT rating,version,updated_at FROM ratings WHERE user_id='u' AND album_id='a'`,
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
		if err != nil || !strings.Contains(plan, "INDEX") || strings.Contains(plan, "SCAN ratings") || strings.Contains(plan, "TEMP B-TREE") {
			t.Fatal("rebuilt rating table lost its bounded key/index access", query, plan, err)
		}
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id='u'`); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ratings`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("user cascade retained votes or tombstones", remaining, err)
	}
}

func TestAlbumRatingsMigrationLateIndexConflictRestoresOldTableDataAndLedger(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumRatings(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedRatingUpgrade(t, db)
	if _, err := db.Exec(`DROP INDEX idx_ratings_album; CREATE INDEX idx_ratings_album ON tags(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("late rating-index conflict was ignored")
	}
	var columns, copied, ledger, retained int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM pragma_table_info('ratings') WHERE name='version'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name='ratings_next'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0024_album_ratings.sql'),
 (SELECT COUNT(*) FROM ratings WHERE (album_id='a' AND rating=5 AND updated_at=-1234) OR (album_id='b' AND rating=2 AND updated_at=5678))`).Scan(&columns, &copied, &ledger, &retained); err != nil || columns != 0 || copied != 0 || ledger != 0 || retained != 2 {
		t.Fatal("failed rebuild retained partial schema or lost old votes", columns, copied, ledger, retained, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_ratings_album; CREATE INDEX idx_ratings_album ON ratings(album_id,rating)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("failed rebuild could not be retried after removing only the fixture collision", err)
	}
}

func TestAlbumRatingsMigrationRejectsRatherThanRoundsHistoricalFractionalVotes(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumRatings(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedRatingUpgrade(t, db)
	// The old SQLite INTEGER affinity plus BETWEEN allowed this non-product value.
	if _, err := db.Exec(`UPDATE ratings SET rating=3.5 WHERE album_id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("fractional historical vote was silently accepted/coerced")
	}
	var value float64
	if err := db.QueryRow(`SELECT rating FROM ratings WHERE album_id='a'`).Scan(&value); err != nil || value != 3.5 {
		t.Fatal("failed strict migration changed the old vote", value, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
}
