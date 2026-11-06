package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

var (
	ErrCreatorMode      = errors.New("use mapped, person with one creator ID, or none")
	ErrCreatorSelection = errors.New("select an existing concrete creator from the album's current source")
)

type AlbumCreator struct {
	AlbumID          string  `json:"album_id"`
	Mode             string  `json:"mode"`
	PersonID         *string `json:"person_id"`
	MappedPersonID   *string `json:"mapped_person_id"`
	OverridePersonID *string `json:"override_person_id"`
	Version          int64   `json:"version"`
}

type AlbumCreatorRequest struct {
	Mode     string  `json:"mode"`
	PersonID *string `json:"person_id"`
}

type albumCreatorState struct {
	value                             AlbumCreator
	source, current, mapped, override sql.NullString
}

// ProjectAlbumCreatorTx shares one source/path identity check between scanner and
// administrator writes. Callers must authorize/fence the resource and transaction.
// Historical preferences can remain stored, but a foreign/unbound ID projects null.
func ProjectAlbumCreatorTx(ctx context.Context, tx *sql.Tx, source sql.NullString, mode string, mapped, override sql.NullString) (sql.NullString, error) {
	var selected sql.NullString
	switch mode {
	case "mapped":
		selected = mapped
	case "person":
		selected = override
	case "none":
		return selected, nil
	default:
		return selected, ErrCreatorMode
	}
	if !source.Valid || !selected.Valid {
		return sql.NullString{}, nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT p.id FROM people p WHERE p.id=? AND p.source_id=? AND p.relative_path IS NOT NULL AND length(p.relative_path)>0 AND p.relative_path!='.'`, selected, source).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullString{}, nil
	}
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: id, Valid: true}, nil
}

func albumCreatorInTx(ctx context.Context, tx *sql.Tx, viewer, id string) (albumCreatorState, error) {
	var state albumCreatorState
	err := tx.QueryRowContext(ctx, `SELECT a.id,a.person_mode,a.person_id,a.mapped_person_id,a.person_override_id,a.version,a.source_id FROM albums a JOIN libraries l ON l.id=a.library_id WHERE `+AlbumVisible+` AND a.id=?`, viewer, id).Scan(&state.value.AlbumID, &state.value.Mode, &state.current, &state.mapped, &state.override, &state.value.Version, &state.source)
	if err != nil {
		return state, err
	}
	state.value.PersonID = nullableMetadataString(state.current)
	state.value.MappedPersonID = nullableMetadataString(state.mapped)
	state.value.OverridePersonID = nullableMetadataString(state.override)
	return state, nil
}

func (s *Service) AdminAlbumCreator(ctx context.Context, current auth.Session, id string) (AlbumCreator, error) {
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return AlbumCreator{}, err
	}
	defer tx.Rollback()
	state, err := albumCreatorInTx(ctx, tx, actor.ID, id)
	return state.value, err
}

func (s *Service) SetAlbumCreator(ctx context.Context, current auth.Session, id string, version int64, input AlbumCreatorRequest, key string) (AlbumCreator, error) {
	switch input.Mode {
	case "mapped", "none":
		if input.PersonID != nil {
			return AlbumCreator{}, ErrCreatorMode
		}
	case "person":
		if input.PersonID == nil {
			return AlbumCreator{}, ErrCreatorMode
		}
		if _, _, err := normalizedGroups([]string{*input.PersonID}); err != nil {
			return AlbumCreator{}, ErrCreatorSelection
		}
	default:
		return AlbumCreator{}, ErrCreatorMode
	}
	tx, actor, err := s.adminTx(ctx, current)
	if err != nil {
		return AlbumCreator{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	data, _ := json.Marshal(struct {
		ID      string              `json:"id"`
		Version int64               `json:"version"`
		Input   AlbumCreatorRequest `json:"input"`
	}{id, version, input})
	command := store.Command{Actor: actor.ID, Key: key, Operation: "album.creator.set", Metadata: string(data)}
	var result AlbumCreator
	if found, err := command.Replay(ctx, tx, now, &result); found || err != nil {
		return result, err
	}
	state, err := albumCreatorInTx(ctx, tx, actor.ID, id)
	if err != nil {
		return AlbumCreator{}, err
	}
	if state.value.Version != version {
		return AlbumCreator{}, auth.ErrConflict
	}
	var override sql.NullString
	if input.Mode == "person" {
		override = sql.NullString{String: *input.PersonID, Valid: true}
	}
	projected, err := ProjectAlbumCreatorTx(ctx, tx, state.source, input.Mode, state.mapped, override)
	if err != nil {
		return AlbumCreator{}, err
	}
	if input.Mode == "person" && !projected.Valid {
		return AlbumCreator{}, ErrCreatorSelection
	}
	if state.value.Mode != input.Mode || state.override != override || state.current != projected {
		if version == math.MaxInt64 {
			return AlbumCreator{}, auth.ErrConflict
		}
		var affected int64
		if state.current != projected {
			affected, err = revokeScopes(ctx, tx, `u.role='admin' OR u.id IN (SELECT ug.user_id FROM user_groups ug JOIN library_access la ON la.group_id=ug.group_id WHERE la.library_id=(SELECT library_id FROM albums WHERE id=?))`, []any{id}, now)
			if err != nil {
				return AlbumCreator{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE albums SET person_mode=?,person_override_id=?,person_id=?,version=version+1,updated_at=max(updated_at,?) WHERE id=?`, input.Mode, override, projected, now.UnixMilli(), id); err != nil {
			return AlbumCreator{}, err
		}
		if err := JournalAlbumTx(ctx, tx, id, now); err != nil {
			return AlbumCreator{}, err
		}
		if err := store.Audit(ctx, tx, actor.ID, "album_creator_changed", "album", id, map[string]any{"mode": input.Mode, "previous_person_id": nullableMetadataString(state.current), "person_id": nullableMetadataString(projected), "override_person_id": nullableMetadataString(override), "affected_users": affected, "version": version + 1}, now); err != nil {
			return AlbumCreator{}, err
		}
	}
	updated, err := albumCreatorInTx(ctx, tx, actor.ID, id)
	if err != nil {
		return AlbumCreator{}, err
	}
	result = updated.value
	if err := command.Save(ctx, tx, result, now); err != nil {
		return AlbumCreator{}, err
	}
	return result, tx.Commit()
}
