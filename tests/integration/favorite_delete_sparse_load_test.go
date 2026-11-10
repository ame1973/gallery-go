//go:build favorite_delete_load

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func TestFavoriteCollectionDeleteSparseSubtreeAt100K(t *testing.T) {
	f := newCollectionFixture(t)
	ctx := t.Context()
	root, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Sparse root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Outside root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `WITH RECURSIVE counter(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM counter WHERE n<10000)
 INSERT INTO favorite_collections(id,user_id,name,created_at,updated_at)
 SELECT printf('sparse-folder-%05d',n),?,printf('Unrelated %05d',n),0,0 FROM counter`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO libraries(id,name,created_at) VALUES('sparse-favorite-library','Sparse',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `WITH RECURSIVE counter(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM counter WHERE n<100000)
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at)
 SELECT printf('sparse-favorite-%06d',n),'sparse-favorite-library','Sparse',0,0 FROM counter`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `WITH RECURSIVE counter(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM counter WHERE n<100000)
 INSERT INTO favorites(user_id,album_id,collection_id,created_at,updated_at,version)
 SELECT ?,printf('sparse-favorite-%06d',n),CASE WHEN n=100000 THEN ? ELSE ? END,0,0,1 FROM counter`, f.owner.User.ID, root.ID, outside.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	previewCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	started := time.Now()
	preview, err := f.service.PreviewFavoriteCollectionDeletion(previewCtx, f.owner, root.ID, access.FavoriteDeleteMoveToDefault)
	previewElapsed := time.Since(started)
	cancel()
	if err != nil {
		t.Fatal("sparse confirmation failed", previewElapsed, err)
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	started = time.Now()
	err = f.service.DeleteFavoriteCollection(deleteCtx, f.owner, root.ID, root.Version, access.FavoriteDeleteMoveToDefault, preview.ConfirmationToken, store.NewID())
	deleteElapsed := time.Since(started)
	cancel()
	if err != nil {
		t.Fatal("sparse confirmed delete failed", deleteElapsed, err)
	}
	var folders, untouched, moved, favoriteEvents, folderEvents int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE user_id=?),
 (SELECT COUNT(*) FROM favorites WHERE user_id=? AND collection_id=? AND favorited=1 AND version=1),
 (SELECT COUNT(*) FROM favorites WHERE user_id=? AND album_id='sparse-favorite-100000' AND collection_id IS NULL AND favorited=1 AND version=2),
 (SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND kind='favorite' AND operation='upsert'),
 (SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND kind='favorite_collection' AND operation='delete')`, f.owner.User.ID, f.owner.User.ID, outside.ID, f.owner.User.ID, f.owner.User.ScopeID, f.owner.User.ScopeID).Scan(&folders, &untouched, &moved, &favoriteEvents, &folderEvents); err != nil || folders != 10001 || untouched != 99999 || moved != 1 || favoriteEvents != 1 || folderEvents != 1 {
		t.Fatal("sparse deletion touched unrelated private data or lost its events", folders, untouched, moved, favoriteEvents, folderEvents, err)
	}
	fmt.Printf("favorite-delete sparse 100k preview=%s commit=%s\n", previewElapsed, deleteElapsed)
}
