package access

import (
	"errors"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestAlbumViewsMonotonicServerTimeAndOverflowFailClosed(t *testing.T) {
	s, owner := fixture(t)
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('view-clock-library','Clock',0);
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES('view-clock-album','view-clock-library','Clock',0,0)`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 1, 2, 3, 456789123, time.UTC)
	s.now = func() time.Time { return now }
	first, err := s.RecordAlbumView(t.Context(), owner, "view-clock-album", store.NewID())
	if err != nil || first.LastViewedAt == nil || !first.LastViewedAt.Equal(now.Truncate(time.Millisecond)) {
		t.Fatal("view time was not normalized to server UTC milliseconds", first, err)
	}
	now = now.Add(-time.Hour)
	second, err := s.RecordAlbumView(t.Context(), owner, "view-clock-album", store.NewID())
	if err != nil || second.ViewCount != 2 || second.LastViewedAt == nil || !second.LastViewedAt.Equal(*first.LastViewedAt) {
		t.Fatal("clock regression moved the personal view instant backward", second, first, err)
	}
	var updated int64
	if err := s.db.QueryRow(`SELECT updated_at FROM albums WHERE id='view-clock-album'`).Scan(&updated); err != nil || updated != first.LastViewedAt.UnixMilli() {
		t.Fatal("clock regression moved shared album update time backward", updated, err)
	}
	for _, invalid := range []time.Time{time.Date(0, 12, 31, 0, 0, 0, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		now = invalid
		if _, err := s.RecordAlbumView(t.Context(), owner, "view-clock-album", store.NewID()); err == nil {
			t.Fatal("out-of-range server clock recorded a view", invalid)
		}
	}
	now = *first.LastViewedAt
	for _, mutation := range []struct{ apply, restore string }{
		{`UPDATE album_views SET version=9223372036854775807 WHERE album_id='view-clock-album'`, `UPDATE album_views SET version=3 WHERE album_id='view-clock-album'`},
		{`UPDATE albums SET version=9223372036854775807 WHERE id='view-clock-album'`, `UPDATE albums SET version=3 WHERE id='view-clock-album'`},
		{`UPDATE album_views SET view_count=9223372036854775807 WHERE album_id='view-clock-album'`, `UPDATE album_views SET view_count=2 WHERE album_id='view-clock-album'`},
	} {
		if _, err := s.db.Exec(mutation.apply); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordAlbumView(t.Context(), owner, "view-clock-album", store.NewID()); !errors.Is(err, auth.ErrConflict) {
			t.Fatal("view or shared version/counter overflow was accepted", mutation.apply, err)
		}
		if _, err := s.db.Exec(mutation.restore); err != nil {
			t.Fatal(err)
		}
	}
	current, err := s.AlbumView(t.Context(), owner, "view-clock-album")
	if err != nil || current.ViewCount != 2 || current.Version != 3 || current.LastViewedAt == nil || !current.LastViewedAt.Equal(*first.LastViewedAt) {
		t.Fatal("rejected clock or overflow request changed the personal state", current, err)
	}
	var total, version int64
	if err := s.db.QueryRow(`SELECT view_count,version FROM albums WHERE id='view-clock-album'`).Scan(&total, &version); err != nil || total != 2 || version != 3 {
		t.Fatal("rejected view changed the shared total or version", total, version, err)
	}
}
