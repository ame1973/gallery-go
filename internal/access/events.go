package access

import (
	"context"
	"database/sql"
	"time"
)

// Group deletion changes policy versions as well as membership. Journal each
// resulting policy, including its persistent restriction flag, before removing
// edges. SQL keeps memory bounded even when a group protects many albums.
func journalGroupPolicyRemovals(ctx context.Context, tx *sql.Tx, groupID string, now time.Time) error {
	var floor int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM change_log`).Scan(&floor); err != nil {
		return err
	}
	for _, query := range []string{
		`INSERT INTO change_log(actor_scope,kind,resource_id,operation,version,created_at,payload_json)
		 SELECT 'system','library_access',l.id,'upsert',l.version+1,?,json_object(
		 'library_id',l.id,'version',l.version+1,'group_ids',json((
		 SELECT json_group_array(group_id) FROM (SELECT group_id FROM library_access
		 WHERE library_id=l.id AND group_id!=? ORDER BY group_id))))
		 FROM libraries l WHERE l.id IN (SELECT library_id FROM library_access WHERE group_id=?)`,
		`INSERT INTO change_log(actor_scope,kind,resource_id,operation,version,created_at,payload_json)
		 SELECT 'system','album_access',a.id,'upsert',a.version+1,?,json_object(
		 'album_id',a.id,'version',a.version+1,'restricted',json(CASE a.access_restricted WHEN 1 THEN 'true' ELSE 'false' END),
		 'group_ids',json((SELECT json_group_array(group_id) FROM (SELECT group_id FROM album_access
		 WHERE album_id=a.id AND group_id!=? ORDER BY group_id))))
		 FROM albums a WHERE a.id IN (SELECT album_id FROM album_access WHERE group_id=?)`,
	} {
		if _, err := tx.ExecContext(ctx, query, now.UnixMilli(), groupID, groupID); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO outbox_events(id,change_seq,created_at)
	 SELECT 'policy:'||seq,seq,? FROM change_log WHERE seq>?`, now.UnixMilli(), floor)
	return err
}
