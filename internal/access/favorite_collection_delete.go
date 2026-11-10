package access

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"strconv"

	"gallerygo/internal/auth"
)

const (
	FavoriteDeleteMoveToDefault = "move_to_default"
	FavoriteDeleteRemove        = "remove_favorites"
)

var ErrFavoriteCollectionDelete = errors.New("choose move_to_default or remove_favorites and use a valid confirmation token")

// FavoriteCollectionDeletionPreview binds a destructive choice to the current
// owned subtree, without exposing inaccessible album identities or their count.
type FavoriteCollectionDeletionPreview struct {
	Action                   string `json:"action"`
	ConfirmationToken        string `json:"confirmation_token"`
	IncludesDescendants      bool   `json:"includes_descendants"`
	HiddenItemsMayBeAffected bool   `json:"hidden_items_may_be_affected"`
}

func validFavoriteDeleteAction(action string) bool {
	return action == FavoriteDeleteMoveToDefault || action == FavoriteDeleteRemove
}

// UNION makes the recursive walk terminate even if stored parent links are corrupt.
// Keep the current parent as the outer loop to seek its owned children, rather
// than rescanning all of an account's folders at every level.
const favoriteDeleteSubtreeSQL = `WITH RECURSIVE subtree(id) AS (
 SELECT id FROM favorite_collections WHERE user_id=? AND id=?
 UNION
 SELECT child.id FROM subtree parent CROSS JOIN favorite_collections child INDEXED BY idx_collection_owner_parent_page
 WHERE child.user_id=? AND child.parent_id=parent.id
)`

// Folder and favorite IDs are length-delimited in the MAC and never returned.
const favoriteCollectionDeleteSnapshotSQL = favoriteDeleteSubtreeSQL + `
SELECT 'collection' AS kind,c.id AS object_id,COALESCE(c.parent_id,'') AS relation,c.version AS version
 FROM subtree s CROSS JOIN favorite_collections c INDEXED BY uq_collection_owner_id
 WHERE c.user_id=? AND c.id=s.id
UNION ALL
SELECT 'favorite',f.album_id,f.collection_id,f.version
 FROM subtree s CROSS JOIN favorites f INDEXED BY idx_favorites_collection
 WHERE f.user_id=? AND f.collection_id=s.id
ORDER BY kind,object_id`

// A corrupt parent cycle or cross-owner ancestor must not turn deletion of one
// folder into a guessed repair of a different subtree. UNION terminates cycles.
const favoriteDeleteAncestrySQL = `WITH RECURSIVE ancestors(id) AS (
 SELECT ?
 UNION
 SELECT c.parent_id FROM ancestors a CROSS JOIN favorite_collections c INDEXED BY uq_collection_owner_id
 WHERE c.user_id=? AND c.id=a.id AND c.parent_id IS NOT NULL
)
SELECT EXISTS(SELECT 1 FROM ancestors a CROSS JOIN favorite_collections c INDEXED BY uq_collection_owner_id
 WHERE c.user_id=? AND c.id=a.id AND c.parent_id IS NULL)`

func checkFavoriteDeleteAncestry(ctx context.Context, tx *sql.Tx, actor string, folder FavoriteCollection) error {
	if folder.ParentID == nil {
		return nil
	}
	var rooted bool
	if err := tx.QueryRowContext(ctx, favoriteDeleteAncestrySQL, folder.ID, actor, actor).Scan(&rooted); err != nil {
		return err
	}
	if !rooted {
		return errors.New("stored favorite collection ancestry is invalid")
	}
	return nil
}

func favoriteDeleteMACField(mac hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = mac.Write(length[:])
	_, _ = io.WriteString(mac, value)
}

func favoriteCollectionDeleteMAC(ctx context.Context, tx *sql.Tx, secret []byte, actor auth.Session, id, action string) ([]byte, error) {
	mac := hmac.New(sha256.New, secret)
	for _, field := range []string{"favorite-collection-delete-v2", actor.Token, actor.User.ID, actor.User.ScopeID, id, action} {
		favoriteDeleteMACField(mac, field)
	}
	rows, err := tx.QueryContext(ctx, favoriteCollectionDeleteSnapshotSQL, actor.User.ID, id, actor.User.ID, actor.User.ID, actor.User.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, objectID, relation string
		var version int64
		if err := rows.Scan(&kind, &objectID, &relation, &version); err != nil {
			return nil, err
		}
		for _, field := range []string{kind, objectID, relation, strconv.FormatInt(version, 10)} {
			favoriteDeleteMACField(mac, field)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return mac.Sum(nil), nil
}

// PreviewFavoriteCollectionDeletion uses a deferred read snapshot, so hashing a
// large subtree never holds the SQLite writer. The first authorized preview may
// initialize one installation-private random key in a short standalone write.
func (s *Service) PreviewFavoriteCollectionDeletion(ctx context.Context, current auth.Session, id, action string) (FavoriteCollectionDeletionPreview, error) {
	if !validFavoriteDeleteAction(action) {
		return FavoriteCollectionDeletionPreview{}, ErrFavoriteCollectionDelete
	}
	preview, missing, err := s.favoriteDeletePreviewSnapshot(ctx, current, id, action)
	if err != nil || !missing {
		return preview, err
	}
	var secret [sha256.Size]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return FavoriteCollectionDeletionPreview{}, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO favorite_delete_confirmation_secret(id,secret) VALUES(1,?) ON CONFLICT(id) DO NOTHING`, secret[:]); err != nil {
		return FavoriteCollectionDeletionPreview{}, err
	}
	preview, missing, err = s.favoriteDeletePreviewSnapshot(ctx, current, id, action)
	if err != nil {
		return FavoriteCollectionDeletionPreview{}, err
	}
	if missing {
		return FavoriteCollectionDeletionPreview{}, errors.New("favorite deletion confirmation key is unavailable")
	}
	return preview, nil
}

func favoriteDeleteSecret(ctx context.Context, tx *sql.Tx) ([]byte, error) {
	var secret []byte
	if err := tx.QueryRowContext(ctx, `SELECT secret FROM favorite_delete_confirmation_secret WHERE id=1`).Scan(&secret); err != nil {
		return nil, err
	}
	var zero [sha256.Size]byte
	if len(secret) != sha256.Size || bytes.Equal(secret, zero[:]) {
		return nil, errors.New("invalid favorite deletion confirmation key")
	}
	return secret, nil
}

func (s *Service) favoriteDeletePreviewSnapshot(ctx context.Context, current auth.Session, id, action string) (FavoriteCollectionDeletionPreview, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return FavoriteCollectionDeletionPreview{}, false, err
	}
	defer tx.Rollback()
	actor, err := s.auth.SessionInTx(ctx, tx, current)
	if err != nil {
		return FavoriteCollectionDeletionPreview{}, false, err
	}
	folder, err := ownedFavoriteCollection(ctx, tx, actor.User.ID, id)
	if err != nil {
		return FavoriteCollectionDeletionPreview{}, false, err
	}
	if err := checkFavoriteDeleteAncestry(ctx, tx, actor.User.ID, folder); err != nil {
		return FavoriteCollectionDeletionPreview{}, false, err
	}
	secret, err := favoriteDeleteSecret(ctx, tx)
	if errors.Is(err, sql.ErrNoRows) {
		return FavoriteCollectionDeletionPreview{}, true, nil
	}
	if err != nil {
		return FavoriteCollectionDeletionPreview{}, false, err
	}
	mac, err := favoriteCollectionDeleteMAC(ctx, tx, secret, actor, id, action)
	if err != nil {
		return FavoriteCollectionDeletionPreview{}, false, err
	}
	return FavoriteCollectionDeletionPreview{Action: action, ConfirmationToken: hex.EncodeToString(mac), IncludesDescendants: true, HiddenItemsMayBeAffected: true}, false, nil
}
