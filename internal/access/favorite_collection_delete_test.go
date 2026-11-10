package access

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/store"
)

func TestFavoriteCollectionDeletionPreviewBindsActorSessionActionAndSubtree(t *testing.T) {
	s, owner, viewer := ratingFixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Preview root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateFavoriteCollection(ctx, owner, "Preview child", &root.ID, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := s.CreateFavoriteCollection(ctx, owner, "Unrelated root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteMoveToDefault)
	if err != nil || preview.Action != FavoriteDeleteMoveToDefault || !preview.IncludesDescendants || !preview.HiddenItemsMayBeAffected {
		t.Fatal("preview lost the explicit action or subtree warning", preview, err)
	}
	if data, err := hex.DecodeString(preview.ConfirmationToken); err != nil || len(data) != 32 || strings.Contains(preview.ConfirmationToken, owner.Token) {
		t.Fatal("preview did not produce an opaque bounded token", err)
	}
	encoded, err := json.Marshal(preview)
	if err != nil || strings.Contains(string(encoded), root.ID) || strings.Contains(string(encoded), child.ID) || strings.Contains(string(encoded), "metadata-album") || strings.Contains(string(encoded), owner.Token) {
		t.Fatal("preview exposed source identities or session secrets", err)
	}
	repeat, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteMoveToDefault)
	if err != nil || repeat != preview {
		t.Fatal("unchanged snapshot produced an unstable confirmation", repeat, err)
	}
	remove, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil || remove.ConfirmationToken == preview.ConfirmationToken {
		t.Fatal("different destructive actions shared a confirmation", remove, err)
	}
	_, err = s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, "cancel")
	requireError(t, err, ErrFavoriteCollectionDelete)
	_, err = s.PreviewFavoriteCollectionDeletion(ctx, viewer, root.ID, FavoriteDeleteRemove)
	requireError(t, err, sql.ErrNoRows)
	forged := viewer
	forged.ID, forged.User = owner.ID, owner.User
	_, err = s.PreviewFavoriteCollectionDeletion(ctx, forged, root.ID, FavoriteDeleteRemove)
	requireError(t, err, sql.ErrNoRows)
	second, err := s.auth.Login(ctx, "owner", testPassword, "192.0.2.1", "second session", "")
	if err != nil {
		t.Fatal(err)
	}
	otherSession, err := s.PreviewFavoriteCollectionDeletion(ctx, second, root.ID, FavoriteDeleteMoveToDefault)
	if err != nil || otherSession.ConfirmationToken == preview.ConfirmationToken {
		t.Fatal("confirmation was not bound to the session", err)
	}
	if _, err := s.CreateFavoriteCollection(ctx, owner, "Outside child", &outside.ID, store.NewID()); err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteMoveToDefault)
	if err != nil || unchanged != preview {
		t.Fatal("unrelated hierarchy changed the confirmation", err)
	}
	if _, err := s.SetAlbumFavorite(ctx, owner, "metadata-album", 1, true, &child.ID, store.NewID()); err != nil {
		t.Fatal(err)
	}
	assigned, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteMoveToDefault)
	if err != nil || assigned.ConfirmationToken == preview.ConfirmationToken {
		t.Fatal("new descendant membership did not invalidate confirmation", err)
	}
	if _, err := s.RenameFavoriteCollection(ctx, owner, child.ID, child.Version, "Renamed child", store.NewID()); err != nil {
		t.Fatal(err)
	}
	renamed, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteMoveToDefault)
	if err != nil || renamed.ConfirmationToken == assigned.ConfirmationToken {
		t.Fatal("descendant revision did not invalidate confirmation", err)
	}
}

func TestFavoriteCollectionDeletionPreviewReadSnapshotAllowsConcurrentWriter(t *testing.T) {
	s, owner := fixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Snapshot root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	actor, err := s.auth.SessionInTx(ctx, tx, owner)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := favoriteDeleteSecret(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := favoriteCollectionDeleteMAC(ctx, tx, secret, actor, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	writerCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.CreateFavoriteCollection(writerCtx, owner, "Concurrent child", &root.ID, store.NewID())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("read snapshot blocked an independent SQLite writer", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("independent writer did not complete")
	}
	stillBefore, err := favoriteCollectionDeleteMAC(ctx, tx, secret, actor, root.ID, FavoriteDeleteRemove)
	if err != nil || !bytes.Equal(before, stillBefore) {
		t.Fatal("one read snapshot mixed hierarchy revisions", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	after, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil || after.ConfirmationToken == hex.EncodeToString(before) {
		t.Fatal("fresh preview missed the committed child", err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := s.PreviewFavoriteCollectionDeletion(cancelled, owner, root.ID, FavoriteDeleteRemove); !errors.Is(err, context.Canceled) {
		t.Fatal("preview ignored request cancellation", err)
	}
}
