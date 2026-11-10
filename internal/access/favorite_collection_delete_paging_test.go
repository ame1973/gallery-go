package access

import (
	"fmt"
	"testing"

	"gallerygo/internal/store"
)

func TestDeleteFavoriteCollectionPagesManyItemsAndDeepHierarchy(t *testing.T) {
	s, owner := fixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Deep root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.PrepareContext(ctx, `INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at,updated_at) VALUES(?,?,?,?,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	parent := root.ID
	for index := 0; index < 2000; index++ {
		id := fmt.Sprintf("delete-depth-%04d", index)
		if _, err := statement.ExecContext(ctx, id, owner.User.ID, parent, "Nested"); err != nil {
			statement.Close()
			t.Fatal(err)
		}
		parent = id
	}
	statement.Close()
	if _, err := tx.ExecContext(ctx, `INSERT INTO libraries(id,name,created_at) VALUES('delete-paging-library','Paging',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `WITH RECURSIVE counter(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM counter WHERE n<260)
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at) SELECT printf('delete-page-%03d',n),'delete-paging-library','Paging',0,0 FROM counter`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `WITH RECURSIVE counter(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM counter WHERE n<260)
 INSERT INTO favorites(user_id,album_id,collection_id,created_at,updated_at,version) SELECT ?,printf('delete-page-%03d',n),?,0,0,1 FROM counter`, owner.User.ID, parent); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteMoveToDefault)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteMoveToDefault, preview.ConfirmationToken, store.NewID()); err != nil {
		t.Fatal("deep confirmed subtree could not be deleted", err)
	}
	var folders, moved, favoriteEvents, folderEvents, missingOutbox int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE user_id=?),
 (SELECT COUNT(*) FROM favorites WHERE user_id=? AND favorited=1 AND collection_id IS NULL AND version=2),
 (SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND kind='favorite' AND operation='upsert' AND resource_id LIKE 'delete-page-%'),
 (SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND kind='favorite_collection' AND operation='delete'),
 (SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE c.actor_scope=? AND c.kind IN ('favorite','favorite_collection') AND o.id IS NULL)`, owner.User.ID, owner.User.ID, owner.User.ScopeID, owner.User.ScopeID, owner.User.ScopeID).Scan(&folders, &moved, &favoriteEvents, &folderEvents, &missingOutbox); err != nil || folders != 0 || moved != 260 || favoriteEvents != 260 || folderEvents != 2001 || missingOutbox != 0 {
		t.Fatal("bounded pages or deep hierarchy lost personal rows or events", folders, moved, favoriteEvents, folderEvents, missingOutbox, err)
	}
	rows, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() || rows.Err() != nil {
		t.Fatal("subtree deletion left a foreign-key violation", rows.Err())
	}
}
