package access

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func albumRatingOrderFixture(t *testing.T) personFixture {
	t.Helper()
	f := albumSelectionFixture(t)
	if _, err := f.svc.db.Exec(`INSERT INTO tags(id,name) VALUES('rating-order-tag','Rank fixture')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"rank-a-unrated", "rank-z-unrated", "rank-a-five", "rank-z-five", "rank-half", "rank-third", "rank-one", "rank-hidden"} {
		if _, err := f.svc.db.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,access_restricted,created_at,updated_at)
 VALUES(?,'person-library','person-source','person-one',?,?,0,0)`, id, id, id == "rank-hidden"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.db.Exec(`INSERT INTO album_tags(album_id,tag_id) VALUES(?,'rating-order-tag')`, id); err != nil {
			t.Fatal(err)
		}
	}
	// Owned SQL setup proves native trigger/AVG semantics, not network mutations.
	// Real rating replacement/clear/delete producers are exercised separately.
	for _, vote := range []struct {
		id     string
		voter  auth.Session
		rating int
	}{
		{"rank-a-five", f.owner, 5}, {"rank-a-five", f.base, 5}, {"rank-a-five", f.both, 5},
		{"rank-z-five", f.owner, 5}, {"rank-half", f.owner, 1}, {"rank-half", f.base, 4},
		{"rank-third", f.owner, 1}, {"rank-third", f.base, 1}, {"rank-third", f.both, 5},
		{"rank-one", f.owner, 1}, {"rank-hidden", f.owner, 5},
	} {
		if _, err := f.svc.db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES(?,?,?,0)`, vote.voter.User.ID, vote.id, vote.rating); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestAlbumRatingOrderUsesNativeMeanNullLastAndReversedIDTies(t *testing.T) {
	f := albumRatingOrderFixture(t)
	want := []string{"rank-z-five", "rank-a-five", "rank-half", "rank-third", "rank-one", "rank-z-unrated", "rank-a-unrated"}
	for _, filter := range []AlbumFilter{
		{TagID: "rating-order-tag"},
		{LibraryID: "person-library", TagID: "rating-order-tag"},
		{SourceID: "person-source", TagID: "rating-order-tag"},
		{PersonID: "person-one", TagID: "rating-order-tag"},
		{LibraryID: "person-library", SourceID: "person-source", PersonID: "person-one", TagID: "rating-order-tag"},
	} {
		filter.Sort = "top_rated"
		items, err := f.svc.Albums(t.Context(), f.both, filter, AlbumPosition{}, 100)
		if err != nil || !slices.Equal(albumSelectionIDs(items), want) {
			t.Fatal("native mean/null/ties/filter-before-limit order differs", filter, albumSelectionIDs(items), want, err)
		}
		for _, item := range items {
			detail, err := f.svc.Album(t.Context(), f.both, item.ID)
			if err != nil || !reflect.DeepEqual(item, detail) || item.MediaCount != 0 || item.CoverMediaID != nil {
				t.Fatal("ranked cards changed shared authorized detail/count/cover", item, detail, err)
			}
			var native *float64
			if err := f.svc.db.QueryRow(`SELECT AVG(rating) FROM ratings WHERE album_id=?`, item.ID).Scan(&native); err != nil || !reflect.DeepEqual(native, item.RatingAvg) {
				t.Fatal("rank score is not the native published mean", item, native, err)
			}
		}
		if items[3].RatingAvg == nil || math.Float64bits(*items[3].RatingAvg) != math.Float64bits(7.0/3) {
			t.Fatal("repeating mean was rounded for ordering", items[3])
		}
		seen, after := []string{}, (AlbumPosition{})
		for range 9 {
			page, err := f.svc.Albums(t.Context(), f.both, filter, after, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			seen = append(seen, page[0].ID)
			after = AlbumPosition{ID: page[0].ID, Key: AlbumRatingKey(page[0].RatingAvg)}
			parsed, err := after.orderKey(filter)
			if err != nil || len(after.Key) > 32 {
				t.Fatal("public mean did not fit its complete canonical position", after, err)
			}
			if page[0].RatingAvg != nil && math.Float64bits(parsed.(float64)) != math.Float64bits(*page[0].RatingAvg) {
				t.Fatal("cursor changed the full published binary64 value", page[0], after)
			}
		}
		if !slices.Equal(seen, want) {
			t.Fatal("native rating pages lost or repeated equal/unknown scores", seen, want)
		}
	}
	for _, filter := range []AlbumFilter{
		{SourceID: "other-person-source", TagID: "rating-order-tag"},
		{PersonID: "unknown", TagID: "rating-order-tag"},
		{TagID: "unknown"},
	} {
		filter.Sort = "top_rated"
		items, err := f.svc.Albums(t.Context(), f.owner, filter, AlbumPosition{}, 30)
		if err != nil || items == nil || len(items) != 0 {
			t.Fatal("contradictory rank selectors leaked global metadata", filter, items, err)
		}
	}
}

func TestAlbumRatingOrderFrozenPositionSurvivesRealRerateAndDeletedAnchor(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		f := albumRatingOrderFixture(t)
		filter := AlbumFilter{TagID: "rating-order-tag", Sort: "top_rated"}
		first, err := f.svc.Albums(t.Context(), f.both, filter, AlbumPosition{}, 1)
		if err != nil || len(first) != 1 || first[0].ID != "rank-z-five" {
			t.Fatal(first, err)
		}
		after := AlbumPosition{ID: first[0].ID, Key: AlbumRatingKey(first[0].RatingAvg)}
		if deleted {
			// Remove only this fixture's empty card; its vote FK cascade is real.
			if _, err := f.svc.db.Exec(`DELETE FROM albums WHERE id='rank-z-five'`); err != nil {
				t.Fatal(err)
			}
		} else if _, err := f.svc.SetAlbumRating(t.Context(), f.owner, "rank-z-five", 1, new(1), store.NewID()); err != nil {
			t.Fatal(err)
		}
		next, err := f.svc.Albums(t.Context(), f.both, filter, after, 1)
		if err != nil || len(next) != 1 || next[0].ID != "rank-a-five" {
			t.Fatal("continuation reread a mutable/deleted score instead of its public key", deleted, next, err)
		}
	}
}

func TestAlbumRatingOrderRejectsNoncanonicalUnknownAndNonfinitePositions(t *testing.T) {
	filter := AlbumFilter{Sort: "top_rated"}
	for _, key := range []string{"", "0", "5", "n:NQ", "r:", "r:NaN", "r:+Inf", "r:-Inf", "r:Infinity", "r:1e309", "r:1.0", "r:1e0", "r:+1", "r:01", "r:-0", "r:0.0", "r:0.5", "r:6", "r:-1", "r:5.000000000000001", "r: 2", "r:2\n", "r:" + strings.Repeat("1", 33)} {
		if _, err := (AlbumPosition{ID: "album", Key: key}).orderKey(filter); !errors.Is(err, ErrAlbumPosition) {
			t.Fatal("invalid/noncanonical native mean position accepted", key, err)
		}
	}
	for _, value := range []float64{0, 1, 2.5, 7.0 / 3, math.Nextafter(1, 2), math.Nextafter(5, 4), 5} {
		key := AlbumRatingKey(&value)
		parsed, err := (AlbumPosition{ID: "album", Key: key}).orderKey(filter)
		if err != nil || math.Float64bits(parsed.(float64)) != math.Float64bits(value) {
			t.Fatal("canonical native mean did not roundtrip exactly", value, key, parsed, err)
		}
	}
	if AlbumRatingKey(nil) != "r:0" || AlbumRatingKey(new(math.Copysign(0, -1))) != "r:0" {
		t.Fatal("null score has more than one zero encoding")
	}
	for _, other := range []string{"", "latest", "oldest", "name_asc", "name_desc"} {
		if _, err := (AlbumPosition{ID: "album", Key: "r:5"}).orderKey(AlbumFilter{Sort: other}); !errors.Is(err, ErrAlbumPosition) {
			t.Fatal("rating position was accepted for an unrelated order", other, err)
		}
	}
	if _, err := (AlbumPosition{}).orderKey(filter); err != nil {
		t.Fatal("first ranked page required an invented anchor", err)
	}
	if key := AlbumRatingKey(new(7.0 / 3)); key != "r:"+strconv.FormatFloat(7.0/3, 'g', -1, 64) {
		t.Fatal("cursor used a rounded display value", key)
	}
}

func TestAlbumRatingOrderFreshVisibilityAndSourceWarningsRemainIndependent(t *testing.T) {
	f := albumRatingOrderFixture(t)
	filter := AlbumFilter{TagID: "rating-order-tag", SourceID: "person-source", Sort: "top_rated"}
	for _, change := range []struct{ apply, restore string }{
		{`UPDATE sources SET is_active=0 WHERE id='person-source'`, `UPDATE sources SET is_active=1 WHERE id='person-source'`},
		{`UPDATE libraries SET is_active=0 WHERE id='person-library'`, `UPDATE libraries SET is_active=1 WHERE id='person-library'`},
		{`UPDATE sources SET library_id='other-person-library' WHERE id='person-source'`, `UPDATE sources SET library_id='person-library' WHERE id='person-source'`},
	} {
		if _, err := f.svc.db.Exec(change.apply); err != nil {
			t.Fatal(err)
		}
		for _, actor := range []auth.Session{f.owner, f.base} {
			items, err := f.svc.Albums(t.Context(), actor, filter, AlbumPosition{}, 30)
			if err != nil || len(items) != 0 {
				t.Fatal("rank index widened inactive/inconsistent source visibility", items, err)
			}
		}
		if _, err := f.svc.db.Exec(change.restore); err != nil {
			t.Fatal(err)
		}
	}
	membership(t, f.svc, f.owner, f.base)
	items, err := f.svc.Albums(t.Context(), f.base, filter, AlbumPosition{}, 30)
	if err != nil || len(items) != 0 {
		t.Fatal("ranked pages trusted a previously granted session", items, err)
	}
}
