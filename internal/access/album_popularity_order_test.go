package access

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func albumPopularityFixture(t *testing.T) personFixture {
	t.Helper()
	f := albumSelectionFixture(t)
	if _, err := f.svc.db.Exec(`INSERT INTO tags(id,name) VALUES('popular-tag','Popular fixture')`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pop-a-zero", "pop-z-zero", "pop-a-high", "pop-z-high", "pop-mid", "pop-low", "pop-hidden"} {
		if _, err := f.svc.db.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,access_restricted,created_at,updated_at)
 VALUES(?,'person-library','person-source','person-one',?,?,0,0)`, id, id, id == "pop-hidden"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.db.Exec(`INSERT INTO album_tags(album_id,tag_id) VALUES(?,'popular-tag')`, id); err != nil {
			t.Fatal(err)
		}
	}
	// Isolated persisted counters exercise the real SQLite projection triggers.
	// The separate view producer and HTTP tests drive accepted events and replay.
	for _, view := range []struct {
		album string
		actor auth.Session
		count int64
	}{
		{"pop-a-high", f.owner, 2}, {"pop-a-high", f.base, 3}, {"pop-z-high", f.owner, 5},
		{"pop-mid", f.owner, 3}, {"pop-low", f.owner, 1}, {"pop-hidden", f.owner, 100},
	} {
		if _, err := f.svc.db.Exec(`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version) VALUES(?,?,?,0,2)`, view.actor.User.ID, view.album, view.count); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestAlbumPopularityOrderUsesRealTotalsZeroLastAndReversedIDTies(t *testing.T) {
	f := albumPopularityFixture(t)
	want := []string{"pop-z-high", "pop-a-high", "pop-mid", "pop-low", "pop-z-zero", "pop-a-zero"}
	for _, filter := range []AlbumFilter{
		{TagID: "popular-tag"},
		{LibraryID: "person-library", TagID: "popular-tag"},
		{SourceID: "person-source", TagID: "popular-tag"},
		{PersonID: "person-one", TagID: "popular-tag"},
		{LibraryID: "person-library", SourceID: "person-source", PersonID: "person-one", TagID: "popular-tag"},
	} {
		filter.Sort = "popular"
		items, err := f.svc.Albums(t.Context(), f.base, filter, AlbumPosition{}, 100)
		if err != nil || !slices.Equal(albumSelectionIDs(items), want) {
			t.Fatal("popularity selection did not order real totals/ID ties before LIMIT", filter, albumSelectionIDs(items), want, err)
		}
		seen, after := []string{}, (AlbumPosition{})
		for range 8 {
			page, err := f.svc.Albums(t.Context(), f.base, filter, after, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			card := page[0]
			detail, err := f.svc.Album(t.Context(), f.base, card.ID)
			if err != nil || !reflect.DeepEqual(card, detail) || card.MediaCount != 0 || card.CoverMediaID != nil {
				t.Fatal("popular card differs from its authorized detail", card, detail, err)
			}
			var native int64
			if err := f.svc.db.QueryRow(`SELECT COALESCE(SUM(view_count),0) FROM album_views WHERE album_id=?`, card.ID).Scan(&native); err != nil || card.ViewCount != native {
				t.Fatal("popular card does not contain its current retained-account total", card, native, err)
			}
			seen = append(seen, card.ID)
			after = AlbumPosition{ID: card.ID, Key: AlbumPopularityKey(card.ViewCount)}
			if parsed, err := after.orderKey(filter); err != nil || parsed != card.ViewCount {
				t.Fatal("popular page changed its complete public integer count", card, after, parsed, err)
			}
		}
		if !slices.Equal(seen, want) {
			t.Fatal("popular keysets repeated or skipped equal/zero counts", seen, want)
		}
	}
	for _, filter := range []AlbumFilter{{SourceID: "other-person-source", TagID: "popular-tag"}, {PersonID: "unknown", TagID: "popular-tag"}, {TagID: "unknown"}} {
		filter.Sort = "popular"
		items, err := f.svc.Albums(t.Context(), f.owner, filter, AlbumPosition{}, 30)
		if err != nil || items == nil || len(items) != 0 {
			t.Fatal("contradictory popular selectors leaked global metadata", filter, items, err)
		}
	}
}

func TestAlbumPopularityOrderFrozenPositionSurvivesRealViewAndDeletedAnchor(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		f := albumPopularityFixture(t)
		filter := AlbumFilter{TagID: "popular-tag", Sort: "popular"}
		first, err := f.svc.Albums(t.Context(), f.base, filter, AlbumPosition{}, 1)
		if err != nil || len(first) != 1 || first[0].ID != "pop-z-high" || first[0].ViewCount != 5 {
			t.Fatal(first, err)
		}
		after := AlbumPosition{ID: first[0].ID, Key: AlbumPopularityKey(first[0].ViewCount)}
		if deleted {
			if _, err := f.svc.db.Exec(`DELETE FROM albums WHERE id='pop-z-high'`); err != nil {
				t.Fatal(err)
			}
		} else {
			key := store.NewID()
			one, err := f.svc.RecordAlbumView(t.Context(), f.owner, "pop-z-high", key)
			if err != nil || one.ViewCount != 6 {
				t.Fatal("real view did not change the anchor", one, err)
			}
			if replay, err := f.svc.RecordAlbumView(t.Context(), f.owner, "pop-z-high", key); err != nil || !reflect.DeepEqual(replay, one) {
				t.Fatal("view retry changed its historical result", replay, one, err)
			}
		}
		next, err := f.svc.Albums(t.Context(), f.base, filter, after, 1)
		if err != nil || len(next) != 1 || next[0].ID != "pop-a-high" {
			t.Fatal("popular continuation reread a changed/deleted anchor", deleted, next, err)
		}
	}
}

func TestAlbumPopularityOrderReflectsRealAccountDeletion(t *testing.T) {
	f := albumPopularityFixture(t)
	target, err := f.svc.auth.GetUser(t.Context(), f.owner, f.base.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.auth.DeleteUser(t.Context(), f.owner, target.ID, target.Version); err != nil {
		t.Fatal(err)
	}
	filter := AlbumFilter{TagID: "popular-tag", Sort: "popular"}
	items, err := f.svc.Albums(t.Context(), f.both, filter, AlbumPosition{}, 100)
	want := []string{"pop-z-high", "pop-mid", "pop-a-high", "pop-low", "pop-z-zero", "pop-a-zero"}
	if err != nil || !slices.Equal(albumSelectionIDs(items), want) || items[2].ViewCount != 2 {
		t.Fatal("account deletion left an obsolete indexed popularity order", albumSelectionIDs(items), want, err)
	}
}

func TestAlbumPopularityOrderIntersectsPersonalFavoritesAndFreshVisibility(t *testing.T) {
	f := albumPopularityFixture(t)
	if _, err := f.svc.db.Exec(`INSERT INTO favorite_collections(id,user_id,name,parent_id,created_at,updated_at) VALUES
 ('popular-parent',?,'Parent',NULL,0,0),('popular-child',?,'Child','popular-parent',0,0)`, f.base.User.ID, f.base.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, favorite := range []struct {
		id         string
		collection *string
	}{{"pop-z-high", new("popular-parent")}, {"pop-mid", new("popular-child")}, {"pop-low", nil}, {"pop-a-high", nil}} {
		if _, err := f.svc.SetAlbumFavorite(t.Context(), f.base, favorite.id, 1, true, favorite.collection, store.NewID()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.SetAlbumFavorite(t.Context(), f.base, "pop-a-high", 2, false, nil, store.NewID()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		filter AlbumFilter
		want   []string
	}{
		{AlbumFilter{Favorite: "true"}, []string{"pop-z-high", "pop-mid", "pop-low"}},
		{AlbumFilter{Favorite: "false"}, []string{"pop-a-high", "pop-z-zero", "pop-a-zero"}},
		{AlbumFilter{FavoriteCollectionID: "popular-parent"}, []string{"pop-z-high"}},
		{AlbumFilter{FavoriteCollectionID: "popular-child"}, []string{"pop-mid"}},
		{AlbumFilter{FavoriteUnfiled: true}, []string{"pop-low"}},
	} {
		test.filter.TagID, test.filter.Sort = "popular-tag", "popular"
		items, err := f.svc.Albums(t.Context(), f.base, test.filter, AlbumPosition{}, 100)
		if err != nil || !slices.Equal(albumSelectionIDs(items), test.want) {
			t.Fatal("popularity widened a personal favorite selection", test.filter, albumSelectionIDs(items), test.want, err)
		}
	}
	foreign, err := f.svc.Albums(t.Context(), f.both, AlbumFilter{FavoriteCollectionID: "popular-parent", TagID: "popular-tag", Sort: "popular"}, AlbumPosition{}, 100)
	if err != nil || foreign == nil || len(foreign) != 0 {
		t.Fatal("foreign folder leaked through popular ordering", foreign, err)
	}
	filter := AlbumFilter{TagID: "popular-tag", SourceID: "person-source", Sort: "popular"}
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
				t.Fatal("popularity widened inactive or inconsistent source visibility", items, err)
			}
		}
		if _, err := f.svc.db.Exec(change.restore); err != nil {
			t.Fatal(err)
		}
	}
	membership(t, f.svc, f.owner, f.base)
	items, err := f.svc.Albums(t.Context(), f.base, filter, AlbumPosition{}, 30)
	if err != nil || len(items) != 0 {
		t.Fatal("popular pages trusted a revoked grant", items, err)
	}
}

func TestAlbumPopularityOrderRejectsNoncanonicalAndForeignPositions(t *testing.T) {
	filter := AlbumFilter{Sort: "popular"}
	for _, key := range []string{"", "0", "5", "n:NQ", "r:5", "v:", "v:-1", "v:-0", "v:+1", "v:01", "v:1.0", "v:1e0", "v: 1", "v:1\n", "v:9223372036854775808", "v:" + strings.Repeat("1", 33)} {
		if _, err := (AlbumPosition{ID: "album", Key: key}).orderKey(filter); !errors.Is(err, ErrAlbumPosition) {
			t.Fatal("invalid/noncanonical popular position was accepted", key, err)
		}
	}
	for _, value := range []int64{0, 1, 42, 9007199254740993, math.MaxInt64} {
		key := AlbumPopularityKey(value)
		parsed, err := (AlbumPosition{ID: "album", Key: key}).orderKey(filter)
		if err != nil || parsed != value {
			t.Fatal("canonical view count did not roundtrip exactly", value, key, parsed, err)
		}
	}
	for _, other := range []string{"", "latest", "oldest", "name_asc", "name_desc", "top_rated"} {
		if _, err := (AlbumPosition{ID: "album", Key: "v:5"}).orderKey(AlbumFilter{Sort: other}); !errors.Is(err, ErrAlbumPosition) {
			t.Fatal("popular position was accepted for an unrelated order", other, err)
		}
	}
	if _, err := (AlbumPosition{}).orderKey(filter); err != nil {
		t.Fatal("first popular page required an invented anchor", err)
	}
}
