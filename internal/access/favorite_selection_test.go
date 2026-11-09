package access

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestFavoriteAlbumSelectionUsesLiveActorStateAcrossCollectionsAndReceipts(t *testing.T) {
	f := albumSelectionFixture(t)
	if _, err := f.svc.db.Exec(`INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES
 ('selection-folder-one',?,'One',0),('selection-folder-two',?,'Two',0)`, f.base.User.ID, f.base.User.ID); err != nil {
		t.Fatal(err)
	}
	add := func(actor auth.Session, id string, collection *string, key string) AlbumFavorite {
		t.Helper()
		value, err := f.svc.SetAlbumFavorite(t.Context(), actor, id, 1, true, collection, key)
		if err != nil || !value.Favorited || value.Version != 2 {
			t.Fatal("could not create actual personal favorite", id, value, err)
		}
		return value
	}
	add(f.base, "person-open", new("selection-folder-one"), store.NewID())
	otherKey := store.NewID()
	other := add(f.base, "person-other", new("selection-folder-two"), otherKey)
	add(f.base, "person-no-path-album", nil, store.NewID())
	if _, err := f.svc.SetAlbumFavorite(t.Context(), f.base, "person-no-path-album", 2, false, nil, store.NewID()); err != nil {
		t.Fatal(err)
	}
	add(f.both, "person-private", nil, store.NewID())

	selected := func(actor auth.Session, filter AlbumFilter, want ...string) {
		t.Helper()
		items, err := f.svc.Albums(t.Context(), actor, filter, AlbumPosition{}, 101)
		if err != nil || items == nil || !slices.Equal(albumSelectionIDs(items), want) {
			t.Fatal("favorite selection lost actor, current state, ordering or visibility", filter, albumSelectionIDs(items), want, err)
		}
		for _, item := range items {
			detail, err := f.svc.Album(t.Context(), actor, item.ID)
			if err != nil || !reflect.DeepEqual(detail, item) {
				t.Fatal("favorite selector altered the shared authorized card", item, detail, err)
			}
		}
	}
	filter := AlbumFilter{LibraryID: "person-library", SourceID: "person-source", TagID: "selection-common"}
	filter.Favorite = "true"
	selected(f.base, filter, "person-open", "person-other")
	selected(f.both, filter, "person-private")
	selected(f.owner, filter)
	forged := f.base
	forged.User = f.owner.User
	selected(forged, filter, "person-open", "person-other")
	filter.PersonID = "person-one"
	selected(f.base, filter, "person-open")
	selected(f.both, filter, "person-private")
	filter.PersonID = ""
	filter.Favorite = "false"
	selected(f.base, filter, "person-no-path-album")
	selected(f.both, filter, "person-no-path-album", "person-open", "person-other")
	filter.Favorite = ""
	selected(f.base, filter, "person-no-path-album", "person-open", "person-other")

	filter.Favorite = "true"
	for _, order := range []struct {
		name string
		want []string
	}{
		{"", []string{"person-open", "person-other"}},
		{"oldest", []string{"person-open", "person-other"}},
		{"latest", []string{"person-other", "person-open"}},
		{"name_asc", []string{"person-open", "person-other"}},
		{"name_desc", []string{"person-other", "person-open"}},
		{"top_rated", []string{"person-other", "person-open"}},
	} {
		filter.Sort = order.name
		selected(f.base, filter, order.want...)
	}
	filter.Sort = ""
	counts := favoriteCounts(t, f.svc)
	selected(f.base, filter, "person-open", "person-other")
	if favoriteCounts(t, f.svc) != counts {
		t.Fatal("reading favorites wrote private events or receipts")
	}
	if _, err := f.svc.SetAlbumFavorite(t.Context(), f.base, "person-open", 2, true, new("selection-folder-two"), store.NewID()); err != nil {
		t.Fatal(err)
	}
	selected(f.base, filter, "person-open", "person-other")
	if _, err := f.svc.SetAlbumFavorite(t.Context(), f.base, "person-other", 2, false, nil, store.NewID()); err != nil {
		t.Fatal(err)
	}
	if replay, err := f.svc.SetAlbumFavorite(t.Context(), f.base, "person-other", 1, true, new("selection-folder-two"), otherKey); err != nil || !reflect.DeepEqual(replay, other) {
		t.Fatal("historical receipt lost its result", replay, err)
	}
	selected(f.base, filter, "person-open")
	filter.Favorite = "false"
	selected(f.base, filter, "person-no-path-album", "person-other")

	filter.Favorite = "true"
	if err := f.svc.DeleteGroup(t.Context(), f.owner, f.privateGroup.ID, f.privateGroup.Version); err != nil {
		t.Fatal(err)
	}
	selected(f.both, filter)
	var retained int
	if err := f.svc.db.QueryRow(`SELECT COUNT(*) FROM favorites WHERE user_id=? AND album_id='person-private' AND favorited=1`, f.both.User.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("revocation deleted private favorite instead of hiding its album", retained, err)
	}
	for _, change := range []struct{ apply, restore string }{
		{`UPDATE sources SET is_active=0 WHERE id='person-source'`, `UPDATE sources SET is_active=1 WHERE id='person-source'`},
		{`UPDATE libraries SET is_active=0 WHERE id='person-library'`, `UPDATE libraries SET is_active=1 WHERE id='person-library'`},
		{`UPDATE sources SET library_id='other-person-library' WHERE id='person-source'`, `UPDATE sources SET library_id='person-library' WHERE id='person-source'`},
	} {
		if _, err := f.svc.db.Exec(change.apply); err != nil {
			t.Fatal(err)
		}
		selected(f.base, filter)
		if _, err := f.svc.db.Exec(change.restore); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.base.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Albums(t.Context(), f.base, filter, AlbumPosition{}, 30); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("favorite selector trusted an inactive actor", err)
	}
	for _, value := range []string{"TRUE", "False", "1", "0", " true", "true ", "null", "yes"} {
		if !errors.Is((AlbumFilter{Favorite: value}).Validate(), ErrAlbumSelection) {
			t.Fatal("noncanonical favorite selector accepted", value)
		}
	}
}
