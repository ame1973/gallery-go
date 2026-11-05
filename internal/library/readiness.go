package library

import (
	"context"
	"time"
)

// Ready reports only recent committed reachability and this process's current
// root pins. It never probes a NAS from an HTTP request, mutates health, or treats
// a healthy reachability result as published inventory/minimum-file evidence.
func (s *Sources) Ready(ctx context.Context) (bool, error) {
	now := s.clock()
	after := ""
	for {
		rows, err := s.db.QueryContext(ctx, sourceSelect+` WHERE is_active=1 AND id>? ORDER BY id LIMIT 100`, after)
		if err != nil {
			return false, err
		}
		count := 0
		for rows.Next() {
			source, err := s.read(rows)
			if err != nil {
				rows.Close()
				return false, err
			}
			count++
			after = source.ID
			freshAfter := now.Add(-2 * time.Duration(source.PollIntervalSeconds) * time.Second)
			if !source.Configured || source.State != "healthy" || source.LastCheckedAt == nil || !source.LastCheckedAt.After(freshAfter) || source.LastCheckedAt.After(now) {
				rows.Close()
				return false, nil
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		if count == 0 {
			return true, nil
		}
	}
}
