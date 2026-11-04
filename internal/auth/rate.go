package auth

import (
	"context"
	"database/sql"
	"time"

	"gallerygo/internal/store"
)

const rateWindow = 15 * time.Minute

func (s *Service) reserveLogin(ctx context.Context, username, ip string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Millisecond)
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_rate_limits WHERE window_started_at<=?`, now.Add(-rateWindow).UnixMilli()); err != nil {
		return err
	}
	for _, bucket := range []struct {
		key   string
		limit int
	}{{"ip:" + digest(ip), 50}, {"user:" + digest(username), 10}} {
		var attempts int
		if err := tx.QueryRowContext(ctx, `INSERT INTO auth_rate_limits(bucket,attempts,window_started_at) VALUES(?,1,?) ON CONFLICT(bucket) DO UPDATE SET attempts=attempts+1 RETURNING attempts`, bucket.key, now.UnixMilli()).Scan(&attempts); err != nil {
			return err
		}
		if attempts > bucket.limit {
			if err := store.Audit(ctx, tx, "", "login_rate_limited", "authentication", "", map[string]string{"username": username, "ip_address": ip}, now); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			return ErrRateLimited
		}
	}
	return tx.Commit()
}

func (s *Service) successfulLogin(ctx context.Context, tx *sql.Tx, username, ip string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_rate_limits WHERE bucket=?`, "user:"+digest(username)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE auth_rate_limits SET attempts=max(0,attempts-1) WHERE bucket=? AND window_started_at>?`, "ip:"+digest(ip), now.Add(-rateWindow).UnixMilli())
	return err
}

func (s *Service) loginFailure(ctx context.Context, userID, username, ip string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := store.Audit(ctx, tx, userID, "login_failed", "authentication", "", map[string]string{"username": username, "ip_address": ip}, s.now().UTC().Truncate(time.Millisecond)); err != nil {
		return err
	}
	return tx.Commit()
}
