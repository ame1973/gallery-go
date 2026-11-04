package jobs

import (
	"context"
	"time"
)

type Worker struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Slots      int       `json:"slots"`
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

func (q *Queue) RegisterWorker(ctx context.Context, id, role string, slots int) error {
	if !keyPattern.MatchString(id) || (role != "serve" && role != "worker") || slots < 1 || slots > 16 {
		return ErrInvalidJob
	}
	now := q.clock().UnixMilli()
	_, err := q.db.ExecContext(ctx, `INSERT INTO workers(id,role,slots,state,started_at,last_seen_at) VALUES(?,?,?,'running',?,?)`, id, role, slots, now, now)
	return err
}

func (q *Queue) WorkerHeartbeat(ctx context.Context, id string) error {
	result, err := q.db.ExecContext(ctx, `UPDATE workers SET last_seen_at=max(last_seen_at,?) WHERE id=? AND state='running'`, q.clock().UnixMilli(), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	return nil
}

func (q *Queue) StopWorker(ctx context.Context, id string) error {
	_, err := q.db.ExecContext(ctx, `UPDATE workers SET state='stopped',last_seen_at=max(last_seen_at,?) WHERE id=?`, q.clock().UnixMilli(), id)
	return err
}

func (q *Queue) Available(ctx context.Context) (bool, error) {
	var available bool
	err := q.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workers WHERE state='running' AND last_seen_at>?)`, q.clock().Add(-q.policy.Lease).UnixMilli()).Scan(&available)
	return available, err
}

func (q *Queue) Workers(ctx context.Context, after string, limit int) ([]Worker, error) {
	if limit < 1 || limit > 101 {
		return nil, ErrInvalidJob
	}
	rows, err := q.db.QueryContext(ctx, `SELECT id,role,slots,state,started_at,last_seen_at FROM workers WHERE id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Worker, 0)
	for rows.Next() {
		var worker Worker
		var started, last int64
		if err := rows.Scan(&worker.ID, &worker.Role, &worker.Slots, &worker.State, &started, &last); err != nil {
			return nil, err
		}
		worker.StartedAt, worker.LastSeenAt = time.UnixMilli(started).UTC(), time.UnixMilli(last).UTC()
		items = append(items, worker)
	}
	return items, rows.Err()
}
