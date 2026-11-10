package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeAlbumPopularityOrder(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeAlbumViews(t)
	data, err := fs.ReadFile(migrations.Files, "0030_album_views.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0030_album_views.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func TestAlbumPopularityOrderMigrationPreservesTotalsAndAddsOrderedIndexes(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumPopularityOrder(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteFolderSelection(t, db)
	if _, err := db.Exec(`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version)
 VALUES('u','a',2,100,3),('v','a',3,101,4),('u','c',1,102,2)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var totals, views, favorites, history, outbox, indexes, ledger, violations int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM albums WHERE (id='a' AND view_count=5) OR (id='c' AND view_count=1)),
 (SELECT COUNT(*) FROM album_views WHERE (user_id='u' AND album_id='a' AND view_count=2 AND version=3) OR (user_id='v' AND album_id='a' AND view_count=3 AND version=4)),
 (SELECT COUNT(*) FROM favorites WHERE (user_id='u' AND album_id='a' AND collection_id='b-child' AND version=3) OR (user_id='v' AND album_id='a' AND favorited=1)),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND payload_json='{"historical":"unchanged"}'),
 (SELECT COUNT(*) FROM outbox_events WHERE id='rating-history'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name IN('idx_album_view_page','idx_album_library_view_page','idx_album_source_view_page','idx_album_person_view_page')),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0031_album_popularity_order.sql'),
 (SELECT COUNT(*) FROM pragma_foreign_key_check)`).Scan(&totals, &views, &favorites, &history, &outbox, &indexes, &ledger, &violations); err != nil || totals != 2 || views != 2 || favorites != 2 || history != 1 || outbox != 1 || indexes != 4 || ledger != 1 || violations != 0 {
		t.Fatal("popularity migration changed existing totals, rows, events or foreign keys", totals, views, favorites, history, outbox, indexes, ledger, violations, err)
	}
	for _, test := range []struct{ query, index string }{
		{`SELECT id FROM albums INDEXED BY idx_album_view_page WHERE (view_count,id)<(5,'z') ORDER BY view_count DESC,id DESC LIMIT 11`, "idx_album_view_page"},
		{`SELECT id FROM albums INDEXED BY idx_album_library_view_page WHERE library_id='l' AND (view_count,id)<(5,'z') ORDER BY view_count DESC,id DESC LIMIT 11`, "idx_album_library_view_page"},
		{`SELECT id FROM albums INDEXED BY idx_album_source_view_page WHERE source_id='s' AND (view_count,id)<(5,'z') ORDER BY view_count DESC,id DESC LIMIT 11`, "idx_album_source_view_page"},
		{`SELECT id FROM albums INDEXED BY idx_album_person_view_page WHERE person_id='p' AND (view_count,id)<(5,'z') ORDER BY view_count DESC,id DESC LIMIT 11`, "idx_album_person_view_page"},
	} {
		rows, err := db.Query(`EXPLAIN QUERY PLAN ` + test.query)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			plan.WriteString(detail)
			plan.WriteByte('\n')
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !strings.Contains(plan.String(), test.index) || strings.Contains(plan.String(), "TEMP B-TREE") || strings.Contains(test.query, "OFFSET") {
			t.Fatal("popularity migration lost a reverse ordered seek", test.query, plan.String(), err)
		}
	}
}

func TestAlbumPopularityOrderMigrationLateIndexFailureRollsBackAndRetries(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumPopularityOrder(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteFolderSelection(t, db)
	if _, err := db.Exec(`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version) VALUES('u','a',2,100,3);
 CREATE INDEX idx_album_person_view_page ON tags(id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("late popularity index conflict was ignored")
	}
	var indexes, collision, ledger, views, total, history int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM sqlite_schema WHERE name IN('idx_album_view_page','idx_album_library_view_page','idx_album_source_view_page')),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name='idx_album_person_view_page' AND tbl_name='tags'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0031_album_popularity_order.sql'),
 (SELECT COUNT(*) FROM album_views WHERE user_id='u' AND album_id='a' AND view_count=2 AND version=3),
 (SELECT view_count FROM albums WHERE id='a'),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND payload_json='{"historical":"unchanged"}')`).Scan(&indexes, &collision, &ledger, &views, &total, &history); err != nil || indexes != 0 || collision != 1 || ledger != 0 || views != 1 || total != 2 || history != 1 {
		t.Fatal("failed popularity migration left partial indexes or changed existing data", indexes, collision, ledger, views, total, history, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_album_person_view_page`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("popularity migration could not retry after removing the fixture collision", err)
	}
}
