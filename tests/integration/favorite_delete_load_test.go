//go:build favorite_delete_load

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

// This host-dependent load check uses a fresh temporary DB and the unchanged
// production 10-second metadata deadline. It is not a real NAS benchmark.
func TestFavoriteCollectionDelete100KMeetsMetadataDeadline(t *testing.T) {
	for _, action := range []string{access.FavoriteDeleteMoveToDefault, access.FavoriteDeleteRemove} {
		t.Run(action, func(t *testing.T) { favoriteDelete100K(t, action) })
	}
}

func favoriteDelete100K(t *testing.T, action string) {
	t.Helper()
	favorited, eventOperation := false, "delete"
	if action == access.FavoriteDeleteMoveToDefault {
		favorited, eventOperation = true, "upsert"
	}
	f := newCollectionFixture(t)
	ctx := t.Context()
	root, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Load root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO libraries(id,name,created_at) VALUES('favorite-delete-load','Load',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `WITH RECURSIVE counter(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM counter WHERE n<100000)
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at)
 SELECT printf('favorite-delete-%06d',n),'favorite-delete-load','Load',0,0 FROM counter`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `WITH RECURSIVE counter(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM counter WHERE n<100000)
 INSERT INTO favorites(user_id,album_id,collection_id,created_at,updated_at,version)
 SELECT ?,printf('favorite-delete-%06d',n),?,0,0,1 FROM counter`, f.owner.User.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	previewCtx, cancelPreview := context.WithTimeout(ctx, 10*time.Second)
	started := time.Now()
	preview, err := f.service.PreviewFavoriteCollectionDeletion(previewCtx, f.owner, root.ID, action)
	previewElapsed := time.Since(started)
	cancelPreview()
	if err != nil {
		t.Fatal("100k confirmation exceeded the metadata deadline or failed", previewElapsed, err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER favorite_load_late_failure BEFORE INSERT ON mutation_receipts WHEN NEW.operation='favorite_collection.delete' BEGIN SELECT RAISE(ABORT,'isolated late receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	key := store.NewID()
	deleteCtx, cancelDelete := context.WithTimeout(ctx, 10*time.Second)
	started = time.Now()
	err = f.service.DeleteFavoriteCollection(deleteCtx, f.owner, root.ID, root.Version, action, preview.ConfirmationToken, key)
	rollbackElapsed := time.Since(started)
	cancelDelete()
	if err == nil || !strings.Contains(err.Error(), "isolated late receipt failure") {
		t.Fatal("100k delete failed before its late rollback checkpoint", rollbackElapsed, err)
	}
	var folders, active, events, outbox, receipts int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE id=?),
 (SELECT COUNT(*) FROM favorites WHERE user_id=? AND collection_id=? AND favorited=1 AND version=1),
 (SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND kind='favorite' AND operation=?),
 (SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.actor_scope=? AND c.kind='favorite' AND c.operation=?),
 (SELECT COUNT(*) FROM mutation_receipts WHERE actor_user_id=? AND operation='favorite_collection.delete')`, root.ID, f.owner.User.ID, root.ID, f.owner.User.ScopeID, eventOperation, f.owner.User.ScopeID, eventOperation, f.owner.User.ID).Scan(&folders, &active, &events, &outbox, &receipts); err != nil || folders != 1 || active != 100000 || events != 0 || outbox != 0 || receipts != 0 {
		t.Fatal("100k late failure did not fully roll back", folders, active, events, outbox, receipts, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER favorite_load_late_failure`); err != nil {
		t.Fatal(err)
	}
	deleteCtx, cancelDelete = context.WithTimeout(ctx, 10*time.Second)
	started = time.Now()
	err = f.service.DeleteFavoriteCollection(deleteCtx, f.owner, root.ID, root.Version, action, preview.ConfirmationToken, key)
	commitElapsed := time.Since(started)
	cancelDelete()
	if err != nil {
		t.Fatal("100k confirmed delete did not commit within the metadata deadline", commitElapsed, err)
	}
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE id=?),
 (SELECT COUNT(*) FROM favorites WHERE user_id=? AND collection_id IS NULL AND favorited=? AND version=2),
 (SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND kind='favorite' AND operation=?),
 (SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.actor_scope=? AND c.kind='favorite' AND c.operation=?),
 (SELECT COUNT(*) FROM mutation_receipts WHERE actor_user_id=? AND operation='favorite_collection.delete')`, root.ID, f.owner.User.ID, favorited, f.owner.User.ScopeID, eventOperation, f.owner.User.ScopeID, eventOperation, f.owner.User.ID).Scan(&folders, &active, &events, &outbox, &receipts); err != nil || folders != 0 || active != 100000 || events != 100000 || outbox != 100000 || receipts != 1 {
		t.Fatal("100k delete lost personal tombstones or transactional history", folders, active, events, outbox, receipts, err)
	}
	fmt.Printf("favorite-delete 100k action=%s preview=%s late-rollback=%s commit=%s\n", action, previewElapsed, rollbackElapsed, commitElapsed)
}
