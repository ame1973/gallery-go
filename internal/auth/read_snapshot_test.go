package auth

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestReadSnapshotBindsIdentityWithoutGrantingAuthority(t *testing.T) {
	s, _ := testService(t)
	current := login(t, s)
	for _, field := range []string{"none", "session", "user", "scope"} {
		t.Run(field, func(t *testing.T) {
			bound := current
			switch field {
			case "session":
				bound.ID = "other-session"
			case "user":
				bound.User.ID = "other-user"
			case "scope":
				bound.User.ScopeID = "other-scope"
			}
			ctx := WithReadSnapshot(t.Context(), bound)
			tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			fresh, err := s.SessionInTx(ctx, tx, current)
			if field == "none" {
				if err != nil || fresh.ID != current.ID {
					t.Fatalf("matching binding: session=%s error=%v", fresh.ID, err)
				}
			} else if !errors.Is(err, ErrConflict) {
				t.Fatalf("mismatched binding accepted: %v", err)
			}
		})
	}
	ctx := WithReadSnapshot(t.Context(), current)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	unknown := current
	unknown.Token = token()
	if _, err := s.SessionInTx(ctx, tx, unknown); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("binding accepted unknown token: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(t.Context(), current, current.ID); err != nil {
		t.Fatal(err)
	}
	revoked, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer revoked.Rollback()
	if _, err := s.SessionInTx(ctx, revoked, current); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("binding accepted revoked token: %v", err)
	}
}

func TestReadSnapshotUsesOneCommittedSnapshot(t *testing.T) {
	s, _ := testService(t)
	current := login(t, s)
	ctx := WithReadSnapshot(t.Context(), current)
	read, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	if _, err := s.SessionInTx(ctx, read, current); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, "new-read-scope", current.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionInTx(ctx, read, current); err != nil {
		t.Fatalf("older committed snapshot did not remain usable: %v", err)
	}
	if err := read.Rollback(); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Rollback()
	if _, err := s.SessionInTx(ctx, fresh, current); !errors.Is(err, ErrConflict) {
		t.Fatalf("new snapshot accepted old scope: %v", err)
	}
	if err := fresh.Rollback(); err != nil {
		t.Fatal(err)
	}
	unbound, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer unbound.Rollback()
	viewer, err := s.SessionInTx(t.Context(), unbound, current)
	if err != nil || viewer.User.ScopeID != "new-read-scope" {
		t.Fatalf("unbound caller did not receive fresh authority: scope=%q error=%v", viewer.User.ScopeID, err)
	}
}

func TestReadSnapshotObservedExpiryStillJournals(t *testing.T) {
	s, _ := testService(t)
	clock := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	current := login(t, s)
	ctx := WithReadSnapshot(t.Context(), current)
	if _, err := s.db.ExecContext(t.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, "new-read-scope", current.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), `UPDATE sessions SET expires_at=? WHERE id=?`, clock.UnixMilli(), current.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := s.SessionInTx(ctx, tx, current); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired stale-scope session: %v", err)
	}
	if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("expired read transaction was not rolled back: %v", err)
	}
	var revoked, audit, changes, outbox int
	err = s.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions WHERE id=? AND revoked_at IS NOT NULL),(SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired' AND resource_id=?),(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete' AND c.resource_id=?)`, current.ID, current.ID, current.ID, current.ID).Scan(&revoked, &audit, &changes, &outbox)
	if err != nil || revoked != 1 || audit != 1 || changes != 1 || outbox != 1 {
		t.Fatalf("expiry journal: revoked=%d audit=%d changes=%d outbox=%d error=%v", revoked, audit, changes, outbox, err)
	}
}
