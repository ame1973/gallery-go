package integration

import (
	"context"
	"errors"
	"slices"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestFavoriteFolderSelectionUsesFreshSessionAndOwnedMembership(t *testing.T) {
	f := newCollectionFixture(t)
	ctx := t.Context()
	root, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Same", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Child", &root.ID, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.service.CreateFavoriteCollection(ctx, f.other, "Same", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('folder-library','Folder library',0);
 INSERT INTO permission_groups(id,name,created_at) VALUES('folder-group','Folder readers',0);
 INSERT INTO library_access(library_id,group_id) VALUES('folder-library','folder-group');
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES
 ('folder-a','folder-library','A',0,0),('folder-b','folder-library','B',0,0),('folder-c','folder-library','C',0,0),('folder-d','folder-library','D',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES(?,'folder-group')`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		viewer     auth.Session
		album      string
		collection *string
	}{
		{f.owner, "folder-a", &root.ID}, {f.owner, "folder-b", &child.ID},
		{f.owner, "folder-c", nil}, {f.other, "folder-d", &other.ID},
	} {
		if _, err := f.service.SetAlbumFavorite(ctx, input.viewer, input.album, 1, true, input.collection, store.NewID()); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(viewer auth.Session, filter access.AlbumFilter, want ...string) {
		t.Helper()
		items, err := f.service.Albums(ctx, viewer, filter, access.AlbumPosition{}, 30)
		got := make([]string, 0, len(items))
		for _, item := range items {
			got = append(got, item.ID)
		}
		if err != nil || items == nil || !slices.Equal(got, want) {
			t.Fatal("folder selection ignored the fresh session, membership or direct folder", filter, got, want, err)
		}
	}
	assert(f.owner, access.AlbumFilter{FavoriteCollectionID: root.ID}, "folder-a")
	assert(f.owner, access.AlbumFilter{FavoriteCollectionID: child.ID}, "folder-b")
	assert(f.owner, access.AlbumFilter{FavoriteUnfiled: true}, "folder-c")
	assert(f.owner, access.AlbumFilter{FavoriteCollectionID: other.ID})
	assert(f.other, access.AlbumFilter{FavoriteCollectionID: other.ID}, "folder-d")
	forged := f.other
	forged.User = f.owner.User
	assert(forged, access.AlbumFilter{FavoriteCollectionID: root.ID})
	assert(forged, access.AlbumFilter{FavoriteCollectionID: other.ID}, "folder-d")
	assert(forged, access.AlbumFilter{FavoriteUnfiled: true})
	for _, filter := range []access.AlbumFilter{
		{FavoriteCollectionID: root.ID, FavoriteUnfiled: true},
		{Favorite: "false", FavoriteCollectionID: root.ID},
		{Favorite: "false", FavoriteUnfiled: true},
	} {
		if _, err := f.service.Albums(ctx, f.owner, filter, access.AlbumPosition{}, 30); !errors.Is(err, access.ErrAlbumSelection) {
			t.Fatal("contradictory service filter was accepted", filter, err)
		}
	}
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Albums(ctx, forged, access.AlbumFilter{FavoriteCollectionID: other.ID}, access.AlbumPosition{}, 30); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("disabled session remained usable through a forged owner DTO", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.service.Albums(cancelled, f.owner, access.AlbumFilter{FavoriteUnfiled: true}, access.AlbumPosition{}, 30); !errors.Is(err, context.Canceled) {
		t.Fatal("folder selection ignored cancellation", err)
	}
}
