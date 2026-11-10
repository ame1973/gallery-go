package access

import (
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestDeleteFavoriteCollectionRejectsChangedMembershipAndFavoriteOverflow(t *testing.T) {
	s, owner, _ := ratingFixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Membership root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	favorite, err := s.SetAlbumFavorite(ctx, owner, "metadata-album", 1, true, &root.ID, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("new membership survived an old confirmation", err)
	}
	if value, err := s.AlbumFavorite(ctx, owner, "metadata-album"); err != nil || value.Version != favorite.Version || !value.Favorited {
		t.Fatal("stale deletion changed the favorite", value, err)
	}
	if _, err := s.db.Exec(`UPDATE favorites SET version=? WHERE user_id=? AND album_id='metadata-album'`, int64(math.MaxInt64), owner.User.ID); err != nil {
		t.Fatal(err)
	}
	preview, err = s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("favorite revision overflow was accepted", err)
	}
	var collection string
	var version int64
	if err := s.db.QueryRow(`SELECT collection_id,version FROM favorites WHERE user_id=? AND album_id='metadata-album'`, owner.User.ID).Scan(&collection, &version); err != nil || collection != root.ID || version != math.MaxInt64 {
		t.Fatal("overflow changed the personal favorite", collection, version, err)
	}
	if _, err := s.FavoriteCollection(ctx, owner, root.ID); err != nil {
		t.Fatal("overflow removed the collection", err)
	}
}

func TestDeleteFavoriteCollectionKeepsMonotonicPersonalTime(t *testing.T) {
	s, owner, _ := ratingFixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Clock root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAlbumFavorite(ctx, owner, "metadata-album", 1, true, &root.ID, store.NewID()); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Truncate(time.Millisecond).Add(24 * time.Hour)
	if _, err := s.db.Exec(`UPDATE favorites SET updated_at=? WHERE user_id=? AND album_id='metadata-album'`, future.UnixMilli(), owner.User.ID); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteMoveToDefault)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteMoveToDefault, preview.ConfirmationToken, store.NewID()); err != nil {
		t.Fatal(err)
	}
	got, err := s.AlbumFavorite(ctx, owner, "metadata-album")
	if err != nil || got.UpdatedAt == nil || !got.UpdatedAt.Equal(future) || got.Version != 3 || !got.Favorited || got.CollectionID != nil {
		t.Fatal("subtree move moved a personal timestamp backward", got, err)
	}
}

func TestFavoriteCollectionDeletionSnapshotUsesOwnerIndexes(t *testing.T) {
	s, owner := fixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Plan root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, query := range []string{
		`CREATE TEMP TABLE favorite_delete_folders(id TEXT PRIMARY KEY,version INTEGER NOT NULL) WITHOUT ROWID`,
		`CREATE TEMP TABLE favorite_delete_items(album_id TEXT PRIMARY KEY,version INTEGER NOT NULL,updated_at INTEGER NOT NULL) WITHOUT ROWID`,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	explain := func(query string, args ...any) string {
		t.Helper()
		rows, err := tx.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(details, "\n")
	}
	const parentSeek = "idx_collection_owner_parent_page (user_id=? AND parent_id=?)"
	const folderSeek = "uq_collection_owner_id (user_id=? AND id=?)"
	const itemSeek = "idx_favorites_collection (user_id=? AND collection_id=?)"
	for _, test := range []struct {
		query string
		args  []any
		want  []string
	}{
		{favoriteCollectionDeleteSnapshotSQL, []any{owner.User.ID, root.ID, owner.User.ID, owner.User.ID, owner.User.ID}, []string{parentSeek, folderSeek, itemSeek}},
		{favoriteDeleteAncestrySQL, []any{root.ID, owner.User.ID, owner.User.ID}, []string{folderSeek}},
		{favoriteDeleteFolderStageSQL, []any{owner.User.ID, root.ID, owner.User.ID, owner.User.ID}, []string{parentSeek, folderSeek}},
		{favoriteDeleteItemStageSQL, []any{owner.User.ID}, []string{itemSeek}},
	} {
		plan := explain(test.query, test.args...)
		for _, want := range test.want {
			if !strings.Contains(plan, want) {
				t.Fatal("subtree deletion lost an indexed actor/parent/collection seek", want, plan)
			}
		}
	}
}

func TestDeleteFavoriteCollectionFailsOnInvalidStoredTimestamp(t *testing.T) {
	s, owner, _ := ratingFixture(t)
	ctx := t.Context()
	root, err := s.CreateFavoriteCollection(ctx, owner, "Invalid time root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAlbumFavorite(ctx, owner, "metadata-album", 1, true, &root.ID, store.NewID()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE favorites SET updated_at=? WHERE user_id=? AND album_id='metadata-album'`, int64(math.MaxInt64), owner.User.ID); err != nil {
		t.Fatal(err)
	}
	preview, err := s.PreviewFavoriteCollectionDeletion(ctx, owner, root.ID, FavoriteDeleteRemove)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFavoriteCollection(ctx, owner, root.ID, root.Version, FavoriteDeleteRemove, preview.ConfirmationToken, store.NewID()); err == nil {
		t.Fatal("out-of-range personal timestamp was published")
	}
	var collection sql.NullString
	var version int64
	if err := s.db.QueryRow(`SELECT collection_id,version FROM favorites WHERE user_id=? AND album_id='metadata-album'`, owner.User.ID).Scan(&collection, &version); err != nil || !collection.Valid || collection.String != root.ID || version != 2 {
		t.Fatal("invalid personal timestamp caused a partial deletion", collection, version, err)
	}
}
