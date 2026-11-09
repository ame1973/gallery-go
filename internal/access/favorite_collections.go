package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"
	"unicode/utf8"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

var (
	ErrFavoriteCollectionParent = errors.New("use one owned collection ID or the root selector")
	ErrFavoriteCollectionName   = errors.New("the exact collection name is already in use under this parent")
)

// FavoriteCollection is personal metadata; it does not expose album IDs or counts.
type FavoriteCollection struct {
	ID        string    `json:"id"`
	ParentID  *string   `json:"parent_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int64     `json:"version"`
}

type FavoriteCollectionFilter struct {
	ParentID *string
	RootOnly bool
}

func (filter FavoriteCollectionFilter) Validate() error {
	if filter.ParentID != nil && (filter.RootOnly || !validFavoriteCollectionID(*filter.ParentID)) {
		return ErrFavoriteCollectionParent
	}
	return nil
}

const favoriteCollectionColumns = `SELECT id,parent_id,name,created_at,updated_at,version FROM favorite_collections`

func scanFavoriteCollection(row interface{ Scan(...any) error }) (FavoriteCollection, error) {
	var value FavoriteCollection
	var created, updated int64
	if err := row.Scan(&value.ID, &value.ParentID, &value.Name, &created, &updated, &value.Version); err != nil {
		return FavoriteCollection{}, err
	}
	if !validFavoriteCollectionID(value.ID) || value.ParentID != nil && !validFavoriteCollectionID(*value.ParentID) {
		return FavoriteCollection{}, errors.New("stored collection identity is outside the public bound")
	}
	if normalized, err := normalizedName(value.Name); err != nil || normalized != value.Name {
		return FavoriteCollection{}, errors.New("stored collection name is outside the public bound")
	}
	value.CreatedAt, value.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	if value.CreatedAt.Year() < 1 || value.CreatedAt.Year() > 9999 || value.UpdatedAt.Year() < 1 || value.UpdatedAt.Year() > 9999 || updated < created || value.Version < 1 {
		return FavoriteCollection{}, errors.New("stored collection revision or time is outside the public bound")
	}
	return value, nil
}

func ownedFavoriteCollection(ctx context.Context, tx *sql.Tx, actor, id string) (FavoriteCollection, error) {
	return scanFavoriteCollection(tx.QueryRowContext(ctx, favoriteCollectionColumns+` WHERE user_id=? AND id=?`, actor, id))
}

// FavoriteCollections lists only this account's folders, independent of album grants.
func (s *Service) FavoriteCollections(ctx context.Context, current auth.Session, filter FavoriteCollectionFilter, after string, limit int) ([]FavoriteCollection, error) {
	if !pageLimit(limit) {
		return nil, ErrGroupSet
	}
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	if len(after) > 64 || !utf8.ValidString(after) {
		return nil, ErrFavoriteCollectionParent
	}
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	query := favoriteCollectionColumns + ` WHERE user_id=?`
	arguments := []any{actor.ID}
	if filter.ParentID != nil {
		if _, err := ownedFavoriteCollection(ctx, tx, actor.ID, *filter.ParentID); err != nil {
			return nil, err
		}
		query += ` AND parent_id=?`
		arguments = append(arguments, *filter.ParentID)
	} else if filter.RootOnly {
		query += ` AND parent_id IS NULL`
	}
	query += ` AND id>? ORDER BY id LIMIT ?`
	arguments = append(arguments, after, limit)
	rows, err := tx.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []FavoriteCollection{}
	for rows.Next() {
		value, err := scanFavoriteCollection(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, value)
	}
	return items, rows.Err()
}

func (s *Service) FavoriteCollection(ctx context.Context, current auth.Session, id string) (FavoriteCollection, error) {
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return FavoriteCollection{}, err
	}
	defer tx.Rollback()
	return ownedFavoriteCollection(ctx, tx, actor.ID, id)
}

func favoriteCollectionNameAvailable(ctx context.Context, tx *sql.Tx, actor string, parent *string, name, exceptID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM favorite_collections WHERE user_id=? AND COALESCE(parent_id,'')=COALESCE(?,'') AND name=? AND id!=?)`, actor, parent, name, exceptID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrFavoriteCollectionName
	}
	return nil
}

// CreateFavoriteCollection writes a private folder and its receipt atomically.
func (s *Service) CreateFavoriteCollection(ctx context.Context, current auth.Session, name string, parent *string, key string) (FavoriteCollection, error) {
	name, err := normalizedName(name)
	if err != nil {
		return FavoriteCollection{}, err
	}
	if parent != nil && !validFavoriteCollectionID(*parent) {
		return FavoriteCollection{}, ErrFavoriteCollectionParent
	}
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return FavoriteCollection{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		Name     string  `json:"name"`
		ParentID *string `json:"parent_id"`
	}{name, parent})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "favorite_collection.create", Metadata: string(data)}
	var value FavoriteCollection
	if found, err := command.Replay(ctx, tx, now, &value); found || err != nil {
		return value, err
	}
	if parent != nil {
		if _, err := ownedFavoriteCollection(ctx, tx, actor.ID, *parent); err != nil {
			return FavoriteCollection{}, err
		}
	}
	if err := favoriteCollectionNameAvailable(ctx, tx, actor.ID, parent, name, ""); err != nil {
		return FavoriteCollection{}, err
	}
	id := store.NewID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at,updated_at,version) VALUES(?,?,?,?,?,?,1)`, id, actor.ID, parent, name, now.UnixMilli(), now.UnixMilli()); err != nil {
		return FavoriteCollection{}, err
	}
	value, err = ownedFavoriteCollection(ctx, tx, actor.ID, id)
	if err != nil {
		return FavoriteCollection{}, err
	}
	if err := store.Change(ctx, tx, actor.ScopeID, "favorite_collection", id, "upsert", value.Version, value, now); err != nil {
		return FavoriteCollection{}, err
	}
	if err := store.Audit(ctx, tx, actor.ID, "favorite_collection_created", "favorite_collection", id, map[string]int64{"version": value.Version}, now); err != nil {
		return FavoriteCollection{}, err
	}
	if err := command.Save(ctx, tx, value, now); err != nil {
		return FavoriteCollection{}, err
	}
	return value, tx.Commit()
}

// RenameFavoriteCollection changes only an owned folder, not its children or items.
func (s *Service) RenameFavoriteCollection(ctx context.Context, current auth.Session, id string, version int64, name, key string) (FavoriteCollection, error) {
	name, err := normalizedName(name)
	if err != nil {
		return FavoriteCollection{}, err
	}
	tx, actor, err := s.readTx(ctx, current)
	if err != nil {
		return FavoriteCollection{}, err
	}
	defer tx.Rollback()
	before, err := ownedFavoriteCollection(ctx, tx, actor.ID, id)
	if err != nil {
		return FavoriteCollection{}, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
		Name    string `json:"name"`
	}{id, version, name})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "favorite_collection.rename", Metadata: string(data)}
	var value FavoriteCollection
	if found, err := command.Replay(ctx, tx, now, &value); found || err != nil {
		return value, err
	}
	if before.Version != version {
		return FavoriteCollection{}, auth.ErrConflict
	}
	if before.Name != name {
		if version == math.MaxInt64 {
			return FavoriteCollection{}, auth.ErrConflict
		}
		if err := favoriteCollectionNameAvailable(ctx, tx, actor.ID, before.ParentID, name, id); err != nil {
			return FavoriteCollection{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE favorite_collections SET name=?,version=?,updated_at=max(updated_at,?) WHERE user_id=? AND id=?`, name, version+1, now.UnixMilli(), actor.ID, id); err != nil {
			return FavoriteCollection{}, err
		}
		value, err = ownedFavoriteCollection(ctx, tx, actor.ID, id)
		if err != nil {
			return FavoriteCollection{}, err
		}
		if err := store.Change(ctx, tx, actor.ScopeID, "favorite_collection", id, "upsert", value.Version, value, now); err != nil {
			return FavoriteCollection{}, err
		}
		if err := store.Audit(ctx, tx, actor.ID, "favorite_collection_renamed", "favorite_collection", id, map[string]int64{"version": value.Version}, now); err != nil {
			return FavoriteCollection{}, err
		}
	} else {
		value = before
	}
	if err := command.Save(ctx, tx, value, now); err != nil {
		return FavoriteCollection{}, err
	}
	return value, tx.Commit()
}
