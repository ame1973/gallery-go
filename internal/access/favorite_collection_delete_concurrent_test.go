package access

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestDeleteFavoriteCollectionSerializesConcurrentRetriesAndFreshAuthority(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		name := "distinct_keys"
		if sameKey {
			name = "same_key"
		}
		t.Run(name, func(t *testing.T) {
			s, owner := fixture(t)
			ctx := t.Context()
			root, err := s.CreateFavoriteCollection(ctx, owner, "Concurrent root", nil, store.NewID())
			if err != nil {
				t.Fatal(err)
			}
			preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
			if err != nil {
				t.Fatal(err)
			}
			key := store.NewID()
			start := make(chan struct{})
			results := make(chan error, 8)
			var workers sync.WaitGroup
			for range 8 {
				workers.Go(func() {
					<-start
					requestKey := key
					if !sameKey {
						requestKey = store.NewID()
					}
					results <- s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, requestKey)
				})
			}
			close(start)
			workers.Wait()
			close(results)
			applied, missing := 0, 0
			for err := range results {
				switch {
				case err == nil:
					applied++
				case errors.Is(err, sql.ErrNoRows):
					missing++
				default:
					t.Fatal("concurrent deletion escaped the receipt or ownership contract", err)
				}
			}
			if sameKey && (applied != 8 || missing != 0) || !sameKey && (applied != 1 || missing != 7) {
				t.Fatal("concurrent effects or historical receipts were duplicated", applied, missing)
			}
			var events, audits, receipts int
			if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND kind='favorite_collection' AND resource_id=? AND operation='delete'),
 (SELECT COUNT(*) FROM audit_events WHERE actor_user_id=? AND event_type='favorite_collection_deleted'),
 (SELECT COUNT(*) FROM mutation_receipts WHERE actor_user_id=? AND operation='favorite_collection.delete')`, owner.User.ScopeID, root.ID, owner.User.ID, owner.User.ID).Scan(&events, &audits, &receipts); err != nil || events != 1 || audits != 1 || receipts != 1 {
				t.Fatal("concurrency duplicated a private event, audit or receipt", events, audits, receipts, err)
			}
			if sameKey {
				if _, err := s.db.Exec(`UPDATE sessions SET revoked_at=? WHERE user_id=?`, time.Now().UnixMilli(), owner.User.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, key); !errors.Is(err, auth.ErrUnauthenticated) {
					t.Fatal("historical receipt bypassed fresh session authority", err)
				}
			}
		})
	}
}
