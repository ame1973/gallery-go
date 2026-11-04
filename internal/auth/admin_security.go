package auth

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"gallerygo/internal/store"
)

func (s *Service) UserSessions(ctx context.Context, current Session, userID, after string, limit int) ([]Session, error) {
	if limit < 1 || limit > 101 {
		return nil, ErrInvalidUser
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err = s.RequireAdmin(ctx, tx, current)
	if err != nil {
		return nil, err
	}
	if _, err := scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE id=?`, userID)); err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	rows, err := tx.QueryContext(ctx, sessionList, userID, now.UnixMilli(), now.Add(-s.policy.Idle).UnixMilli(), after, limit)
	if err != nil {
		return nil, err
	}
	return scanSessions(rows, current.ID)
}

func (s *Service) RevokeUserSession(ctx context.Context, current Session, userID, sessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err = s.RequireAdmin(ctx, tx, current)
	if err != nil {
		return err
	}
	user, err := scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE id=?`, userID))
	if err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Millisecond)
	var id string
	if err := tx.QueryRowContext(ctx, `UPDATE sessions SET revoked_at=? WHERE user_id=? AND id=? AND revoked_at IS NULL RETURNING id`, now.UnixMilli(), userID, sessionID).Scan(&id); err != nil {
		return err
	}
	if err := store.Change(ctx, tx, user.ScopeID, "session", id, "delete", 1, map[string]string{"user_id": userID}, now); err != nil {
		return err
	}
	if err := store.Audit(ctx, tx, current.User.ID, "admin_session_revoked", "session", id, map[string]any{"user_id": userID, "current": id == current.ID}, now); err != nil {
		return err
	}
	return tx.Commit()
}

type AuditEvent struct {
	ID           string          `json:"id"`
	ActorUserID  *string         `json:"actor_user_id"`
	EventType    string          `json:"event_type"`
	ResourceType *string         `json:"resource_type"`
	ResourceID   *string         `json:"resource_id"`
	Detail       json.RawMessage `json:"detail"`
	CreatedAt    time.Time       `json:"created_at"`
}

// Audit returns newest-first keyset pages. Appending events cannot duplicate or
// skip older events between pages, and no unbounded history is loaded into memory.
func (s *Service) Audit(ctx context.Context, current Session, before int64, limit int) ([]AuditEvent, error) {
	if before < 1 || limit < 1 || limit > 101 {
		return nil, ErrInvalidUser
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := s.RequireAdmin(ctx, tx, current); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,actor_user_id,event_type,resource_type,resource_id,detail_json,created_at FROM audit_events WHERE id<? ORDER BY id DESC LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AuditEvent, 0)
	for rows.Next() {
		var event AuditEvent
		var id, created int64
		var detail string
		if err := rows.Scan(&id, &event.ActorUserID, &event.EventType, &event.ResourceType, &event.ResourceID, &detail, &created); err != nil {
			return nil, err
		}
		event.ID, event.CreatedAt = strconv.FormatInt(id, 10), time.UnixMilli(created).UTC()
		event.Detail = json.RawMessage(detail)
		items = append(items, event)
	}
	return items, rows.Err()
}
