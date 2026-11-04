package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// NewID returns a sortable opaque UUIDv7, never an authorization capability.
func NewID() string { return uuid.Must(uuid.NewV7()).String() }

// Change publishes the business event and its durable outbox row in the caller's
// transaction. Secrets and local filesystem paths must never enter this payload.
func Change(ctx context.Context, tx *sql.Tx, scope, kind, id, operation string, version int64, payload any, now time.Time) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO change_log(actor_scope,kind,resource_id,operation,version,payload_json,created_at) VALUES(?,?,?,?,?,?,?)`, scope, kind, id, operation, version, string(data), now.UnixMilli())
	if err != nil {
		return err
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox_events(id,change_seq,created_at) VALUES(?,?,?)`, NewID(), seq, now.UnixMilli())
	return err
}

// Audit is deliberately transaction-bound, so a sensitive write cannot commit
// without its audit record. Callers supply only non-secret structured details.
func Audit(ctx context.Context, tx *sql.Tx, actor, event, resourceType, resourceID string, detail any, now time.Time) error {
	data, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO audit_events(actor_user_id,event_type,resource_type,resource_id,detail_json,created_at) VALUES(?,?,?,?,?,?)`, actor, event, resourceType, resourceID, string(data), now.UnixMilli())
	return err
}
