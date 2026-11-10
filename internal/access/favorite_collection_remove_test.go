package access

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestDeleteFavoriteCollectionMovesOrClearsOwnedSubtreeAtomically(t *testing.T) {
	for _, action := range []string{FavoriteDeleteMoveToDefault, FavoriteDeleteRemove} {
		t.Run(action, func(t *testing.T) {
			s, owner, viewer := ratingFixture(t)
			ctx := t.Context()
			root, err := s.CreateFavoriteCollection(ctx, viewer, "Root", nil, store.NewID())
			if err != nil {
				t.Fatal(err)
			}
			child, err := s.CreateFavoriteCollection(ctx, viewer, "Child", &root.ID, store.NewID())
			if err != nil {
				t.Fatal(err)
			}
			outside, err := s.CreateFavoriteCollection(ctx, viewer, "Outside", nil, store.NewID())
			if err != nil {
				t.Fatal(err)
			}
			for id, collection := range map[string]string{"metadata-album": child.ID, "metadata-other": root.ID, "metadata-empty": outside.ID} {
				if _, err := s.SetAlbumFavorite(ctx, viewer, id, 1, true, &collection, store.NewID()); err != nil {
					t.Fatal(err)
				}
			}
			other, err := s.SetAlbumFavorite(ctx, owner, "metadata-album", 1, true, nil, store.NewID())
			if err != nil {
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
				t.Fatal("fixture failed to hide the formerly authorized album", err)
			}
			preview, err := s.PreviewFavoriteCollectionDeletion(ctx, viewer, root.ID, action)
			if err != nil {
				t.Fatal(err)
			}
			key := store.NewID()
			if err := s.DeleteFavoriteCollection(ctx, viewer, root.ID, root.Version, action, preview.ConfirmationToken, key); err != nil {
				t.Fatal("confirmed owned subtree deletion failed", err)
			}
			for _, id := range []string{root.ID, child.ID} {
				if _, err := s.FavoriteCollection(ctx, viewer, id); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("deleted descendant remains visible", id, err)
				}
				var operation, payload string
				var version int64
				if err := s.db.QueryRow(`SELECT operation,version,payload_json FROM change_log WHERE actor_scope=? AND kind='favorite_collection' AND resource_id=? ORDER BY seq DESC LIMIT 1`, viewer.User.ScopeID, id).Scan(&operation, &version, &payload); err != nil || operation != "delete" || version != 2 || payload != `{"id":"`+id+`"}` {
					t.Fatal("missing private collection tombstone", id, operation, version, payload, err)
				}
			}
			if _, err := s.FavoriteCollection(ctx, viewer, outside.ID); err != nil {
				t.Fatal("unrelated collection was removed", err)
			}
			for _, id := range []string{"metadata-album", "metadata-other"} {
				var favorited bool
				var collection sql.NullString
				var version int64
				if err := s.db.QueryRow(`SELECT favorited,collection_id,version FROM favorites WHERE user_id=? AND album_id=?`, viewer.User.ID, id).Scan(&favorited, &collection, &version); err != nil || favorited != (action == FavoriteDeleteMoveToDefault) || collection.Valid || version != 3 {
					t.Fatal("hidden personal favorite was lost or given the wrong revision", id, favorited, collection, version, err)
				}
				var operation, payload string
				if err := s.db.QueryRow(`SELECT operation,payload_json FROM change_log WHERE actor_scope=? AND kind='favorite' AND resource_id=? ORDER BY seq DESC LIMIT 1`, viewer.User.ScopeID, id).Scan(&operation, &payload); err != nil {
					t.Fatal(err)
				}
				var event AlbumFavorite
				var fields map[string]json.RawMessage
				wantOp := "upsert"
				if action == FavoriteDeleteRemove {
					wantOp = "delete"
				}
				if json.Unmarshal([]byte(payload), &event) != nil || json.Unmarshal([]byte(payload), &fields) != nil || len(fields) != 5 || operation != wantOp || event.AlbumID != id || event.Favorited != favorited || event.CollectionID != nil || event.Version != 3 || event.UpdatedAt == nil {
					t.Fatal("favorite event did not match the private state", operation, payload)
				}
			}
			var unchangedCollection string
			var unchangedVersion int64
			if err := s.db.QueryRow(`SELECT collection_id,version FROM favorites WHERE user_id=? AND album_id='metadata-empty'`, viewer.User.ID).Scan(&unchangedCollection, &unchangedVersion); err != nil || unchangedCollection != outside.ID || unchangedVersion != 2 {
				t.Fatal("unrelated favorite was changed", unchangedCollection, unchangedVersion, err)
			}
			if current, err := s.AlbumFavorite(ctx, owner, "metadata-album"); err != nil || !reflect.DeepEqual(current, other) {
				t.Fatal("another account's favorite changed", current, err)
			}
			var audit, receipts, dangling int
			var auditDetail string
			if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM audit_events WHERE actor_user_id=? AND event_type='favorite_collection_deleted'),
 (SELECT COUNT(*) FROM mutation_receipts WHERE actor_user_id=? AND operation='favorite_collection.delete'),
 (SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE c.actor_scope=? AND c.kind IN ('favorite','favorite_collection') AND o.id IS NULL)`, viewer.User.ID, viewer.User.ID, viewer.User.ScopeID).Scan(&audit, &receipts, &dangling); err != nil || audit != 1 || receipts != 1 || dangling != 0 {
				t.Fatal("audit, receipt or outbox did not co-commit", audit, receipts, dangling, err)
			}
			if err := s.db.QueryRow(`SELECT detail_json FROM audit_events WHERE actor_user_id=? AND event_type='favorite_collection_deleted'`, viewer.User.ID).Scan(&auditDetail); err != nil || strings.Contains(auditDetail, "metadata-") || strings.Contains(auditDetail, preview.ConfirmationToken) {
				t.Fatal("deletion audit exposed hidden IDs or the confirmation", auditDetail, err)
			}
			if err := s.DeleteFavoriteCollection(ctx, viewer, root.ID, root.Version, action, preview.ConfirmationToken, key); err != nil {
				t.Fatal("retry after resource deletion did not replay", err)
			}
			if err := s.DeleteFavoriteCollection(ctx, viewer, root.ID, root.Version, action, preview.ConfirmationToken, store.NewID()); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("a new request replayed a deleted folder", err)
			}
			if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, action, preview.ConfirmationToken, key); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("another actor reused a private receipt", err)
			}
			otherAction := FavoriteDeleteRemove
			if action == FavoriteDeleteRemove {
				otherAction = FavoriteDeleteMoveToDefault
			}
			if err := s.DeleteFavoriteCollection(ctx, viewer, root.ID, root.Version, otherAction, preview.ConfirmationToken, key); !errors.Is(err, store.ErrIdempotencyReuse) {
				t.Fatal("reused key changed the confirmed action", err)
			}
		})
	}
}

func TestDeleteFavoriteCollectionRejectsStaleConfirmationAndOverflow(t *testing.T) {
	s, owner := fixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Conflict root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateFavoriteCollection(ctx, owner, "New child", &root.ID, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("new child did not invalidate the old confirmation", err)
	}
	preview, err = s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteMoveToDefault, preview.ConfirmationToken, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("confirmation authorized a different action", err)
	}
	for _, invalid := range []string{"", strings.Repeat("0", 63), strings.Repeat("X", 64), strings.ToUpper(preview.ConfirmationToken)} {
		if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, invalid, store.NewID()); !errors.Is(err, ErrFavoriteCollectionDelete) {
			t.Fatal("malformed confirmation accepted", invalid, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE favorite_collections SET version=? WHERE id=?`, int64(math.MaxInt64), child.ID); err != nil {
		t.Fatal(err)
	}
	preview, err = s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("overflowing descendant revision was deleted", err)
	}
	if _, err := s.FavoriteCollection(ctx, owner, root.ID); err != nil {
		t.Fatal("failed deletion changed its root", err)
	}
	if _, err := s.db.Exec(`UPDATE favorite_collections SET version=1 WHERE id=?`, child.ID); err != nil {
		t.Fatal(err)
	}
	preview, err = s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET revoked_at=? WHERE id_hash=(SELECT id_hash FROM sessions WHERE user_id=? AND revoked_at IS NULL LIMIT 1)`, time.Now().UnixMilli(), owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("revoked session retained delete authority", err)
	}
}
