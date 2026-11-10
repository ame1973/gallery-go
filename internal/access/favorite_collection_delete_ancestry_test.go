package access

import (
	"testing"

	"gallerygo/internal/store"
)

func TestFavoriteCollectionDeletionRejectsCorruptAncestry(t *testing.T) {
	for _, corruption := range []string{"cycle", "foreign_parent"} {
		t.Run(corruption, func(t *testing.T) {
			s, owner, viewer := ratingFixture(t)
			ctx := t.Context()
			root, err := s.CreateFavoriteCollection(ctx, owner, "Corrupt root", nil, store.NewID())
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.CreateFavoriteCollection(ctx, owner, "Corrupt child", &root.ID, store.NewID())
			if err != nil {
				t.Fatal(err)
			}
			foreign, err := s.CreateFavoriteCollection(ctx, viewer, "Foreign root", nil, store.NewID())
			if err != nil {
				t.Fatal(err)
			}
			preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
			if err != nil {
				t.Fatal(err)
			}
			// Simulate a damaged isolated DB after migration; normal API writes cannot
			// bypass this trigger or create either invalid ancestry.
			if _, err := s.db.Exec(`DROP TRIGGER favorite_collection_parent_guard`); err != nil {
				t.Fatal(err)
			}
			parent := child.ID
			if corruption == "foreign_parent" {
				parent = foreign.ID
			}
			if _, err := s.db.Exec(`UPDATE favorite_collections SET parent_id=? WHERE id=?`, parent, root.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove); err == nil {
				t.Fatal("preview treated corrupted ancestry as a valid subtree")
			}
			if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); err == nil {
				t.Fatal("deletion silently repaired a corrupted hierarchy")
			}
			var folders, events int
			if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE id IN (?,?,?)),
 (SELECT COUNT(*) FROM change_log WHERE kind='favorite_collection' AND operation='delete')`, root.ID, child.ID, foreign.ID).Scan(&folders, &events); err != nil || folders != 3 || events != 0 {
				t.Fatal("corrupt ancestry caused a partial delete or event", folders, events, err)
			}
		})
	}
}
