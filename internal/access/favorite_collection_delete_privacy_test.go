package access

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"testing"

	"gallerygo/internal/store"
)

func TestFavoriteCollectionDeletionPreviewCannotBeGuessedFromClientSession(t *testing.T) {
	s, owner, viewer := ratingFixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, viewer, "Private root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAlbumFavorite(ctx, viewer, "metadata-album", 1, true, &root.ID, store.NewID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLibraryPolicy(ctx, owner, "metadata-library", 2, []string{}); err != nil {
		t.Fatal(err)
	}
	viewer, err = s.auth.Authenticate(ctx, viewer.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AlbumFavorite(ctx, viewer, "metadata-album"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("fixture did not hide the previously authorized favorite", err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, viewer, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	// A client knows its own cookie and can guess a former album ID. A MAC keyed
	// only by that cookie would let it confirm the hidden membership offline.
	for _, prefix := range [][]string{
		{"favorite-collection-delete-v1", viewer.User.ID, viewer.User.ScopeID, root.ID, FavoriteDeleteRemove},
		{"favorite-collection-delete-v2", viewer.Token, viewer.User.ID, viewer.User.ScopeID, root.ID, FavoriteDeleteRemove},
	} {
		guess := hmac.New(sha256.New, []byte(viewer.Token))
		for _, field := range append(prefix, "collection", root.ID, "", "1", "favorite", "metadata-album", root.ID, "2") {
			favoriteDeleteMACField(guess, field)
		}
		if hex.EncodeToString(guess.Sum(nil)) == preview.ConfirmationToken {
			t.Fatal("client-known session secret revealed the guessed hidden membership")
		}
	}
}
