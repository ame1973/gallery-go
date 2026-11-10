package media

import (
	"context"
	"database/sql"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
)

type variantContentState struct {
	storage, hash, contentType, name, revision, preset string
	size, presetVersion, mediaVersion, revisionVersion int64
}

// Read only database state here; opening and hashing files must happen after
// this snapshot is released, followed by a fresh authorization/pin check.
func (s *Service) variantContentState(ctx context.Context, current auth.Session, mediaID, variantID string) (variantContentState, error) {
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return variantContentState{}, err
	}
	defer tx.Rollback()
	var state variantContentState
	err = tx.QueryRowContext(ctx, `SELECT v.storage_path,v.sha256,v.mime,v.size_bytes,m.file_name,v.revision_id,v.preset_key,v.preset_version,m.version,r.version
	 FROM media_variants v JOIN media_revisions r ON r.id=v.revision_id JOIN media m ON m.current_revision_id=r.id AND m.id=r.media_id
	 JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
	 WHERE `+access.MediaVisible+` AND m.id=? AND v.id=? AND v.status='ready'`, viewer.ID, mediaID, variantID).Scan(&state.storage, &state.hash, &state.contentType, &state.size, &state.name, &state.revision, &state.preset, &state.presetVersion, &state.mediaVersion, &state.revisionVersion)
	if err != nil {
		return variantContentState{}, err
	}
	if err := tx.Rollback(); err != nil {
		return variantContentState{}, err
	}
	return state, nil
}

func (s *Service) requireVariantContentState(ctx context.Context, current auth.Session, mediaID, variantID string, expected variantContentState) error {
	actual, err := s.variantContentState(ctx, current, mediaID, variantID)
	if err != nil {
		return err
	}
	if actual != expected {
		return ErrVariantUnavailable
	}
	return nil
}

type originalContentState struct {
	sourceID, entryID, revisionID, sourceName, sourcePath, rootSignature string
	state, availability, mediaAvailability, relative, hash, entryHash    string
	contentType, name                                                    string
	size, mtime, entrySize, entryMtime, sourceConfig, entryConfig        int64
	mediaVersion, entryVersion, revisionVersion                          int64
}

func (s *Service) originalContentState(ctx context.Context, current auth.Session, mediaID string) (originalContentState, error) {
	tx, viewer, err := s.readTx(ctx, current)
	if err != nil {
		return originalContentState{}, err
	}
	defer tx.Rollback()
	// The actual account policy applies even to administrators.
	if !viewer.DownloadOriginal {
		return originalContentState{}, sql.ErrNoRows
	}
	var state originalContentState
	err = tx.QueryRowContext(ctx, `SELECT src.id,e.id,r.id,src.name,src.root_path,src.root_config_sha256,src.config_version,e.source_config_version,src.state,e.availability,e.relative_path,r.source_sha256,COALESCE(e.sha256,''),
	 r.source_size,r.source_mtime_ns,e.size_bytes,e.mtime_ns,COALESCE(m.mime,''),m.file_name,m.availability,m.version,e.version,r.version
	 FROM media m JOIN albums a ON a.id=m.album_id JOIN libraries l ON l.id=a.library_id
	 JOIN media_revisions r ON r.id=m.current_revision_id AND r.media_id=m.id
	 JOIN source_entries e ON e.id=m.source_entry_id AND e.source_id=a.source_id
	 JOIN sources src ON src.id=e.source_id AND src.library_id=l.id
	 WHERE `+access.MediaVisible+` AND m.id=?`, viewer.ID, mediaID).Scan(&state.sourceID, &state.entryID, &state.revisionID, &state.sourceName, &state.sourcePath, &state.rootSignature, &state.sourceConfig, &state.entryConfig, &state.state, &state.availability, &state.relative, &state.hash, &state.entryHash, &state.size, &state.mtime, &state.entrySize, &state.entryMtime, &state.contentType, &state.name, &state.mediaAvailability, &state.mediaVersion, &state.entryVersion, &state.revisionVersion)
	if err != nil {
		return originalContentState{}, err
	}
	if err := tx.Rollback(); err != nil {
		return originalContentState{}, err
	}
	return state, nil
}

func (s *Service) requireOriginalContentState(ctx context.Context, current auth.Session, mediaID string, expected originalContentState) error {
	actual, err := s.originalContentState(ctx, current, mediaID)
	if err != nil {
		return err
	}
	if actual != expected {
		return ErrSourceUnavailable
	}
	return nil
}
