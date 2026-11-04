package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

func normalizedGroups(ids []string) ([]string, string, error) {
	if ids == nil || len(ids) > 100 {
		return nil, "", ErrGroupSet
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	for i, id := range ids {
		if id == "" || len(id) > 64 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) || (i > 0 && ids[i-1] == id) {
			return nil, "", ErrGroupSet
		}
	}
	data, _ := json.Marshal(ids)
	return ids, string(data), nil
}

func knownGroups(ctx context.Context, tx *sql.Tx, data string, count int) error {
	var found int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM permission_groups WHERE id IN (SELECT value FROM json_each(?))`, data).Scan(&found); err != nil {
		return err
	}
	if found != count {
		return ErrUnknownGroup
	}
	return nil
}

func scanGroupIDs(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) > 100 {
		return nil, ErrGroupSet
	}
	return ids, rows.Err()
}

// RevokeSourceScopesTx invalidates old and destination library projections during
// an authorized source configuration change. Existing albums stay fail-closed if
// their library differs until a healthy inventory publication reassigns them.
// The caller must authorize and commit its source/job/events in this transaction.
func RevokeSourceScopesTx(ctx context.Context, tx *sql.Tx, previousLibrary, nextLibrary string, now time.Time) error {
	_, err := revokeScopes(ctx, tx, `u.role='admin' OR u.id IN (
	 SELECT ug.user_id FROM user_groups ug JOIN library_access la ON la.group_id=ug.group_id
	 WHERE la.library_id IN (?,?))`, []any{previousLibrary, nextLibrary}, now)
	return err
}

// RevokeInventoryScopesTx precedes indexed ownership changes. It selects both
// historical album libraries and the configured destination without materializing
// accounts in Go; old-scope revoke events survive the subsequent visibility move.
func RevokeInventoryScopesTx(ctx context.Context, tx *sql.Tx, sourceID string, now time.Time) error {
	_, err := revokeScopes(ctx, tx, `u.role='admin' OR u.id IN (
	 SELECT ug.user_id FROM user_groups ug JOIN library_access la ON la.group_id=ug.group_id
	 WHERE la.library_id IN (SELECT library_id FROM sources WHERE id=? UNION SELECT library_id FROM albums WHERE source_id=?))`, []any{sourceID, sourceID}, now)
	return err
}

// revokeScopes uses bounded-memory SQL for any number of affected accounts. The
// selection is static SQL from this package, never request text. It runs before
// changing grant edges, so both revoked users and new grantees can be selected.
func revokeScopes(ctx context.Context, tx *sql.Tx, selection string, args []any, now time.Time) (int64, error) {
	var floor int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM change_log`).Scan(&floor); err != nil {
		return 0, err
	}
	parameters := append([]any{now.UnixMilli()}, args...)
	if _, err := tx.ExecContext(ctx, `INSERT INTO change_log(actor_scope,kind,resource_id,operation,version,payload_json,created_at)
	 SELECT u.scope_id,'scope',u.id,'revoke',u.version+1,'{"reason":"access_changed"}',? FROM users u WHERE `+selection, parameters...); err != nil {
		return 0, err
	}
	// Private outbox IDs derived from unique change sequences cannot collide with
	// each other or the UUID IDs used by single-resource store.Change calls.
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbox_events(id,change_seq,created_at)
	 SELECT 'scope:'||seq,seq,? FROM change_log WHERE seq>?`, now.UnixMilli(), floor); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE users AS u SET scope_id=lower(hex(randomblob(32))),version=version+1,updated_at=? WHERE `+selection, parameters...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
