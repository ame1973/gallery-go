package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"

	"gallerygo/internal/auth"
)

func TestAlbumViewsAccountDeletionCombinesOverlappingVotesAndPagedViews(t *testing.T) {
	f := newCollectionFixture(t)
	seedAlbumViews(t, f)
	f.db.SetMaxOpenConns(1)
	f.db.SetMaxIdleConns(1)
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at)
 VALUES('view-c','view-library','view-source','C',0,0);
 WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<255)
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at)
 SELECT printf('page-%03d',i),'view-library','Page',0,0 FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES
 (?,'view-a',5,0),(?,'view-b',NULL,0),(?,'view-c',4,0),(?,'view-a',1,0),(?,'view-b',2,0)`,
		f.other.User.ID, f.other.User.ID, f.other.User.ID, f.owner.User.ID, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version) VALUES
 (?,'view-a',3,0,4),(?,'view-b',1,0,2),(?,'view-a',2,0,3)`, f.other.User.ID, f.other.User.ID, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version)
 SELECT ?,id,1,0,2 FROM albums WHERE id LIKE 'page-%'`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := func() [7]int64 {
		t.Helper()
		var got [7]int64
		if err := f.db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM users WHERE id=?),
 (SELECT COUNT(*) FROM album_views WHERE user_id=?),
 (SELECT COUNT(*) FROM ratings WHERE user_id=?),
 (SELECT COUNT(*) FROM albums WHERE (id LIKE 'page-%' OR id IN ('view-a','view-b','view-c')) AND version=1),
 (SELECT SUM(view_count) FROM albums WHERE id LIKE 'page-%' OR id IN ('view-a','view-b','view-c')),
 (SELECT COUNT(*) FROM change_log WHERE kind='album'),
 (SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='album')`,
			f.other.User.ID, f.other.User.ID, f.other.User.ID).Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5], &got[6]); err != nil {
			t.Fatal(err)
		}
		return got
	}
	tempGone := func() {
		t.Helper()
		var count int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM sqlite_temp_master WHERE name='deleting_user_album_ids'`).Scan(&count); err != nil || count != 0 {
			t.Fatal("account deletion left its transaction-local IDs on the pooled connection", count, err)
		}
	}
	before := snapshot()
	if before != [7]int64{1, 258, 3, 259, 262, 0, 0} {
		t.Fatal("overlapping account deletion fixture is invalid", before)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_overlap_delete BEFORE INSERT ON outbox_events
 WHEN EXISTS(SELECT 1 FROM change_log c WHERE c.seq=NEW.change_seq AND c.kind='album' AND c.resource_id='view-c')
 BEGIN SELECT RAISE(ABORT,'injected final album outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.DeleteUser(t.Context(), f.owner, f.other.User.ID, f.other.User.Version); err == nil {
		t.Fatal("late outbox failure did not abort overlapping account deletion")
	}
	if got := snapshot(); got != before {
		t.Fatal("late outbox failure leaked views, votes, accounts or journals", before, got)
	}
	tempGone()
	if _, err := f.db.Exec(`DROP TRIGGER fail_overlap_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE albums SET version=? WHERE id='view-c'`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.DeleteUser(t.Context(), f.owner, f.other.User.ID, f.other.User.Version); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("album version overflow did not fail account deletion", err)
	}
	tempGone()
	if _, err := f.db.Exec(`UPDATE albums SET version=1 WHERE id='view-c'`); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(); got != before {
		t.Fatal("version overflow leaked account deletion changes", before, got)
	}
	if err := f.auth.DeleteUser(t.Context(), f.owner, f.other.User.ID, f.other.User.Version); err != nil {
		t.Fatal(err)
	}
	tempGone()
	var users, views, ratings, totals, versions, events, unique, missing int64
	if err := f.db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM users WHERE id=?),
 (SELECT COUNT(*) FROM album_views WHERE user_id=?),
 (SELECT COUNT(*) FROM ratings WHERE user_id=?),
 (SELECT SUM(view_count) FROM albums WHERE id LIKE 'page-%' OR id IN ('view-a','view-b','view-c')),
 (SELECT COUNT(*) FROM albums WHERE (id LIKE 'page-%' OR id IN ('view-a','view-b','view-c')) AND version=2),
 (SELECT COUNT(*) FROM change_log WHERE kind='album'),
 (SELECT COUNT(DISTINCT resource_id) FROM change_log WHERE kind='album'),
 (SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE c.kind='album' AND o.id IS NULL)`,
		f.other.User.ID, f.other.User.ID, f.other.User.ID).Scan(&users, &views, &ratings, &totals, &versions, &events, &unique, &missing); err != nil {
		t.Fatal(err)
	}
	if got := [8]int64{users, views, ratings, totals, versions, events, unique, missing}; got != [8]int64{0, 0, 0, 2, 259, 259, 259, 0} {
		t.Fatal("account deletion did not publish one final event per affected album", got)
	}
	for _, want := range []struct {
		id      string
		views   int64
		average *float64
	}{{"view-a", 2, new(1.0)}, {"view-b", 0, new(2.0)}, {"view-c", 0, nil}} {
		var body string
		if err := f.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='album' AND resource_id=?`, want.id).Scan(&body); err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		var projection struct {
			ID        string   `json:"id"`
			ViewCount int64    `json:"view_count"`
			RatingAvg *float64 `json:"rating_avg"`
			Version   int64    `json:"version"`
		}
		if err := json.Unmarshal([]byte(body), &fields); err != nil || len(fields) != 14 || json.Unmarshal([]byte(body), &projection) != nil || projection.ID != want.id || projection.ViewCount != want.views || projection.Version != 2 || (projection.RatingAvg == nil) != (want.average == nil) || projection.RatingAvg != nil && *projection.RatingAvg != *want.average {
			t.Fatal("account deletion emitted an incomplete or intermediate shared album", want, body, err)
		}
	}
}

func TestAlbumViewsAccountDeletionRatingOnlyPathCrossover(t *testing.T) {
	for _, count := range []int{128, 129} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newCollectionFixture(t)
			f.db.SetMaxOpenConns(1)
			f.db.SetMaxIdleConns(1)
			if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('rating-crossover','Ratings',0)`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i+1<?)
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at)
 SELECT printf('rating-%03d',i),'rating-crossover','Rating',0,0 FROM n`, count); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at)
 SELECT ?,id,5,0 FROM albums WHERE library_id='rating-crossover'`, f.other.User.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`CREATE TRIGGER fail_crossover_delete BEFORE INSERT ON change_log
 WHEN NEW.kind='album' AND NEW.resource_id=` + fmt.Sprintf("'rating-%03d'", count-1) + `
 BEGIN SELECT RAISE(ABORT,'injected crossover failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := f.auth.DeleteUser(t.Context(), f.owner, f.other.User.ID, f.other.User.Version); err == nil {
				t.Fatal("rating-only deletion ignored its late journal failure")
			}
			var users, votes, untouched, events, temps int
			if err := f.db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM users WHERE id=?),(SELECT COUNT(*) FROM ratings WHERE user_id=?),
 (SELECT COUNT(*) FROM albums WHERE library_id='rating-crossover' AND version=1 AND rating_avg=5),
 (SELECT COUNT(*) FROM change_log WHERE kind='album'),
 (SELECT COUNT(*) FROM sqlite_temp_master WHERE name='deleting_user_album_ids')`, f.other.User.ID, f.other.User.ID).
				Scan(&users, &votes, &untouched, &events, &temps); err != nil || users != 1 || votes != count || untouched != count || events != 0 || temps != 0 {
				t.Fatal("rating-only failure leaked state or temporary IDs", users, votes, untouched, events, temps, err)
			}
			if _, err := f.db.Exec(`DROP TRIGGER fail_crossover_delete`); err != nil {
				t.Fatal(err)
			}
			if err := f.auth.DeleteUser(t.Context(), f.owner, f.other.User.ID, f.other.User.Version); err != nil {
				t.Fatal(err)
			}
			var changed, missing int
			if err := f.db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM albums WHERE library_id='rating-crossover' AND version=2 AND rating_avg IS NULL),
 (SELECT COUNT(*) FROM change_log WHERE kind='album'),
 (SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE c.kind='album' AND o.id IS NULL),
 (SELECT COUNT(*) FROM sqlite_temp_master WHERE name='deleting_user_album_ids')`).Scan(&changed, &events, &missing, &temps); err != nil || changed != count || events != count || missing != 0 || temps != 0 {
				t.Fatal("rating-only crossover lost shared projections, outboxes or cleanup", changed, events, missing, temps, err)
			}
		})
	}
}
