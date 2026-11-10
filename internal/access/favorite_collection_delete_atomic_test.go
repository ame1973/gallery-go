package access

import (
	"database/sql"
	"reflect"
	"testing"

	"gallerygo/internal/store"
)

func TestDeleteFavoriteCollectionRollsBackEveryLateFailure(t *testing.T) {
	s, owner, _ := ratingFixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Atomic root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateFavoriteCollection(ctx, owner, "Atomic child", &root.ID, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.SetAlbumFavorite(ctx, owner, "metadata-album", 1, true, &child.ID, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	counts := func() [5]int64 {
		t.Helper()
		var value [5]int64
		if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections),
 (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),
 (SELECT COUNT(*) FROM audit_events),(SELECT COUNT(*) FROM mutation_receipts)`).Scan(&value[0], &value[1], &value[2], &value[3], &value[4]); err != nil {
			t.Fatal(err)
		}
		return value
	}
	initial := counts()
	key := store.NewID()
	for _, trigger := range []string{
		`CREATE TRIGGER fail_favorite_delete BEFORE INSERT ON change_log WHEN NEW.kind='favorite' AND NEW.operation='delete' BEGIN SELECT RAISE(ABORT,'favorite event failure'); END`,
		`CREATE TRIGGER fail_folder_delete BEFORE INSERT ON change_log WHEN NEW.kind='favorite_collection' AND NEW.operation='delete' BEGIN SELECT RAISE(ABORT,'folder event failure'); END`,
		`CREATE TRIGGER fail_outbox_delete BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log WHERE seq=NEW.change_seq AND kind='favorite_collection' AND operation='delete') BEGIN SELECT RAISE(ABORT,'outbox failure'); END`,
		`CREATE TRIGGER fail_audit_delete BEFORE INSERT ON audit_events WHEN NEW.event_type='favorite_collection_deleted' BEGIN SELECT RAISE(ABORT,'audit failure'); END`,
		`CREATE TRIGGER fail_receipt_delete BEFORE INSERT ON mutation_receipts WHEN NEW.operation='favorite_collection.delete' BEGIN SELECT RAISE(ABORT,'receipt failure'); END`,
	} {
		if _, err := s.db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, key); err == nil {
			t.Fatal("injected failure committed a partial subtree")
		}
		if got := counts(); got != initial {
			t.Fatal("failed deletion leaked a row, event, outbox, audit or receipt", initial, got)
		}
		if value, err := s.AlbumFavorite(ctx, owner, "metadata-album"); err != nil || !reflect.DeepEqual(value, before) {
			t.Fatal("failed deletion changed a favorite", value, err)
		}
		if value, err := s.FavoriteCollection(ctx, owner, child.ID); err != nil || !reflect.DeepEqual(value, child) {
			t.Fatal("failed deletion changed a descendant", value, err)
		}
		for _, name := range []string{"fail_favorite_delete", "fail_folder_delete", "fail_outbox_delete", "fail_audit_delete", "fail_receipt_delete"} {
			if _, err := s.db.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
				t.Fatal(err)
			}
		}
		fresh, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
		if err != nil || fresh != preview {
			t.Fatal("rollback changed the confirmed snapshot", fresh, err)
		}
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, key); err != nil {
		t.Fatal("rollback left temporary state or blocked a valid retry", err)
	}
	if _, err := s.FavoriteCollection(ctx, owner, child.ID); err != sql.ErrNoRows {
		t.Fatal("retry failed to delete the child", err)
	}
}
