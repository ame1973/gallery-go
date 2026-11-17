package jobs

import (
	"context"
	"database/sql"
	"errors"

	"gallerygo/internal/auth"
)

// StatusCounts reports current job rows, not attempts or work units.
type StatusCounts struct {
	Queued    int64 `json:"queued"`
	Running   int64 `json:"running"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
	Cancelled int64 `json:"cancelled"`
	Dead      int64 `json:"dead"`
}

type Metrics struct {
	Jobs StatusCounts `json:"jobs"`
}

// ponytail: grouping scans retained job index entries; add a transactionally
// maintained aggregate only if measured job history makes this too expensive.
const statusCountsQuery = `SELECT status,COUNT(*) FROM jobs GROUP BY status`

func statusCounts(ctx context.Context, tx *sql.Tx) (StatusCounts, error) {
	rows, err := tx.QueryContext(ctx, statusCountsQuery)
	if err != nil {
		return StatusCounts{}, err
	}
	defer rows.Close()
	var counts StatusCounts
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return StatusCounts{}, err
		}
		switch status {
		case "queued":
			counts.Queued = count
		case "running":
			counts.Running = count
		case "succeeded":
			counts.Succeeded = count
		case "failed":
			counts.Failed = count
		case "cancelled":
			counts.Cancelled = count
		case "dead":
			counts.Dead = count
		default:
			return StatusCounts{}, errors.New("invalid job status in database")
		}
	}
	if err := rows.Err(); err != nil {
		return StatusCounts{}, err
	}
	return counts, nil
}

// Metrics rechecks administrator authority in the same snapshot as the counts.
func (a *Admin) Metrics(ctx context.Context, current auth.Session) (Metrics, error) {
	tx, err := a.queue.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Metrics{}, err
	}
	defer tx.Rollback()
	if _, err := a.auth.RequireAdmin(ctx, tx, current); err != nil {
		return Metrics{}, err
	}
	counts, err := statusCounts(ctx, tx)
	if err != nil {
		return Metrics{}, err
	}
	return Metrics{Jobs: counts}, nil
}
