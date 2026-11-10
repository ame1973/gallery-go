package access

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestFavoriteDeletionKeyInitializesOnceAndNeverAppearsInPreview(t *testing.T) {
	s, owner, viewer := ratingFixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Key root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.PreviewFavoriteCollectionDeletion(ctx, viewer, root.ID, FavoriteDeleteRemove)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("foreign actor reached key initialization", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM favorite_delete_confirmation_secret`).Scan(&count); err != nil || count != 0 {
		t.Fatal("unauthorized preview initialized secret state", count, err)
	}
	counts := func() [4]int64 {
		t.Helper()
		var value [4]int64
		if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),
 (SELECT COUNT(*) FROM audit_events),(SELECT COUNT(*) FROM mutation_receipts)`).Scan(&value[0], &value[1], &value[2], &value[3]); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := counts()
	var filename string
	if err := s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&filename); err != nil {
		t.Fatal(err)
	}
	secondDB, err := store.Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	secondAuth, err := auth.New(secondDB, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	second := New(secondDB, secondAuth)
	start := make(chan struct{})
	type outcome struct {
		value FavoriteCollectionDeletionPreview
		err   error
	}
	results := make(chan outcome, 8)
	var workers sync.WaitGroup
	for index := range 8 {
		workers.Go(func() {
			<-start
			service := s
			if index%2 != 0 {
				service = second
			}
			value, err := service.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
			results <- outcome{value, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	var token string
	for result := range results {
		if result.err != nil {
			t.Fatal("concurrent first preview failed", result.err)
		}
		if token != "" && token != result.value.ConfirmationToken {
			t.Fatal("service instances selected different installation keys")
		}
		token = result.value.ConfirmationToken
	}
	var secret []byte
	if err := s.db.QueryRow(`SELECT secret FROM favorite_delete_confirmation_secret WHERE id=1`).Scan(&secret); err != nil || len(secret) != 32 || bytes.Equal(secret, make([]byte, 32)) || token == hex.EncodeToString(secret) {
		t.Fatal("installation key is missing, invalid or directly exposed", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM favorite_delete_confirmation_secret`).Scan(&count); err != nil || count != 1 {
		t.Fatal("concurrent preview duplicated installation keys", count, err)
	}
	if after := counts(); after != before {
		t.Fatal("confirmation generated business events, audits or receipts", before, after)
	}
}

func TestFavoriteDeletionKeyPersistsAcrossIndependentPoolsAndReopen(t *testing.T) {
	s, owner := fixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Reopen root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	var filename string
	if err := s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&filename); err != nil {
		t.Fatal(err)
	}
	open := func() (*sql.DB, *Service) {
		t.Helper()
		db, err := store.Open(ctx, filename)
		if err != nil {
			t.Fatal(err)
		}
		a, err := auth.New(db, auth.DefaultPolicy())
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
		return db, New(db, a)
	}
	secondDB, second := open()
	secondPreview, err := second.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil || secondPreview != preview {
		secondDB.Close()
		t.Fatal("independent database pool changed the confirmation", secondPreview, err)
	}
	if err := secondDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedDB, reopened := open()
	defer reopenedDB.Close()
	reopenedPreview, err := reopened.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil || reopenedPreview != preview {
		t.Fatal("reopened database changed the confirmation", reopenedPreview, err)
	}
	if err := reopened.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); err != nil {
		t.Fatal("reopened service could not use an earlier valid confirmation", err)
	}
}

func TestFavoriteDeletionKeyFailureAndReplacementFailClosed(t *testing.T) {
	s, owner := fixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Failed key root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_favorite_secret BEFORE INSERT ON favorite_delete_confirmation_secret BEGIN SELECT RAISE(ABORT,'isolated key failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove); err == nil {
		t.Fatal("failed key initialization returned a confirmation")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM favorite_delete_confirmation_secret`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed key initialization left a partial secret", count, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_favorite_secret`); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM favorite_delete_confirmation_secret WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing server key did not fail closed as a server error", err)
	}
	if _, err := s.FavoriteCollection(ctx, owner, root.ID); err != nil {
		t.Fatal("missing key removed the collection", err)
	}
	fresh, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil || fresh.ConfirmationToken == preview.ConfirmationToken {
		t.Fatal("replacement key did not invalidate the old confirmation", err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("old confirmation survived replacement of the server key", err)
	}
}
