//go:build album_views_load

package integration

import (
	"context"
	"testing"
	"time"
)

// Run separately without race instrumentation to exercise the production 10s
// metadata deadline on an isolated real SQLite database, not a user's data.
func TestAlbumViewsAccountDelete100KMeetsMetadataDeadline(t *testing.T) {
	runAlbumViewsAccountDelete100K(t, true, false)
}

func TestAlbumViewsAndRatingsAccountDelete100KMeetsMetadataDeadline(t *testing.T) {
	runAlbumViewsAccountDelete100K(t, true, true)
}

func TestAlbumRatingsOnlyAccountDelete100KMeetsMetadataDeadline(t *testing.T) {
	runAlbumViewsAccountDelete100K(t, false, true)
}

func runAlbumViewsAccountDelete100K(t *testing.T, withViews, withRatings bool) {
	t.Helper()
	f := newCollectionFixture(t)
	if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('view-load-library','Load',0);
 WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<99999)
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at)
 SELECT printf('view-load-%06d',i),'view-load-library','Album',0,0 FROM n`); err != nil {
		t.Fatal(err)
	}
	if withViews {
		if _, err := f.db.Exec(`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version)
 SELECT ?,id,1,0,2 FROM albums WHERE id LIKE 'view-load-%'`, f.other.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	if withRatings {
		if _, err := f.db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at)
 SELECT ?,id,5,0 FROM albums WHERE id LIKE 'view-load-%'`, f.other.User.ID); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started := time.Now()
	err := f.auth.DeleteUser(ctx, f.owner, f.other.User.ID, f.other.User.Version)
	elapsed := time.Since(started)
	var users, rows, total, events, missing, ratings, averages int64
	if queryErr := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM users WHERE id=?),(SELECT COUNT(*) FROM album_views WHERE user_id=?),
 (SELECT SUM(view_count) FROM albums WHERE id LIKE 'view-load-%'),
 (SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id LIKE 'view-load-%'),
 (SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE c.kind='album' AND c.resource_id LIKE 'view-load-%' AND o.id IS NULL),
 (SELECT COUNT(*) FROM ratings WHERE user_id=? AND rating IS NOT NULL),
 (SELECT COUNT(*) FROM albums WHERE id LIKE 'view-load-%' AND rating_avg IS NOT NULL)`, f.other.User.ID, f.other.User.ID, f.other.User.ID).Scan(&users, &rows, &total, &events, &missing, &ratings, &averages); queryErr != nil {
		t.Fatal(queryErr)
	}
	wantViews, wantRatings, wantEvents := int64(0), int64(0), int64(100000)
	if withViews {
		wantViews = 100000
	}
	if withRatings {
		wantRatings = 100000
	}
	if err != nil {
		if users != 1 || rows != wantViews || total != wantViews || events != 0 || missing != 0 || ratings != wantRatings || averages != wantRatings {
			t.Fatal("failed deadline leaked a partial account deletion", elapsed, err, users, rows, total, events, missing, ratings, averages)
		}
		t.Fatal("100k account history exceeded the production metadata deadline", elapsed, err)
	}
	if elapsed >= 10*time.Second || users != 0 || rows != 0 || total != 0 || events != wantEvents || missing != 0 || ratings != 0 || averages != 0 {
		t.Fatal("large account deletion lost its deadline, totals or complete journals", elapsed, users, rows, total, events, missing, ratings, averages)
	}
	t.Log("100k account deletion elapsed", elapsed)
}
