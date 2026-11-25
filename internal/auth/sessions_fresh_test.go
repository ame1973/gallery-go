package auth

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestSessionsRejectsStaleCaller(t *testing.T) {
	for _, state := range []string{"revoked", "disabled", "unknown-token", "changed-scope", "changed-session-id", "changed-account-id"} {
		t.Run(state, func(t *testing.T) {
			s, _ := testService(t)
			current := login(t, s)
			other := login(t, s)
			want := ErrUnauthenticated
			switch state {
			case "revoked":
				if err := s.Revoke(t.Context(), current, current.ID); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if _, err := s.db.ExecContext(t.Context(), `UPDATE users SET active=0 WHERE id=?`, current.User.ID); err != nil {
					t.Fatal(err)
				}
			case "unknown-token":
				current.Token = token()
			case "changed-scope":
				if _, err := s.db.ExecContext(t.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, token(), current.User.ID); err != nil {
					t.Fatal(err)
				}
				want = ErrConflict
			case "changed-session-id":
				current.ID = other.ID
				want = ErrConflict
			case "changed-account-id":
				current.User.ID = "different-account"
				want = ErrConflict
			}
			items, err := s.Sessions(t.Context(), current, "", 30)
			if !errors.Is(err, want) || len(items) != 0 {
				t.Fatalf("stale caller returned %d sessions, error=%v, want=%v", len(items), err, want)
			}
			if state == "changed-scope" {
				fresh, err := s.Authenticate(t.Context(), current.Token)
				if err != nil {
					t.Fatal(err)
				}
				items, err = s.Sessions(t.Context(), fresh, "", 30)
				if err != nil || len(items) != 2 {
					t.Fatalf("fresh scope did not restore session list: count=%d error=%v", len(items), err)
				}
				marked := 0
				for _, item := range items {
					if item.Current {
						marked++
						if item.ID != fresh.ID {
							t.Fatal("current marker identified a different session")
						}
					}
				}
				if marked != 1 {
					t.Fatalf("current session markers=%d", marked)
				}
			}
		})
	}
}

func TestSessionsRejectsForgedAccountSnapshot(t *testing.T) {
	s, _ := testService(t)
	owner := login(t, s)
	reader := managedUser(t, s, owner, "reader", "user")
	current := managedLogin(t, s, reader)
	current.User = owner.User
	current.ID = owner.ID
	items, err := s.Sessions(t.Context(), current, "", 30)
	if !errors.Is(err, ErrConflict) || len(items) != 0 {
		t.Fatalf("forged caller returned %d sessions, error=%v", len(items), err)
	}
}

func TestSessionsObservedExpiryIsDurable(t *testing.T) {
	for _, kind := range []string{"idle", "absolute"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := testService(t)
			clock := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
			s.now = func() time.Time { return clock }
			current := login(t, s)
			if kind == "idle" {
				clock = clock.Add(12 * time.Hour)
				login(t, s)
				clock = clock.Add(12 * time.Hour)
			} else {
				login(t, s)
				if _, err := s.db.ExecContext(t.Context(), `UPDATE sessions SET expires_at=? WHERE id=?`, clock.UnixMilli(), current.ID); err != nil {
					t.Fatal(err)
				}
			}
			items, err := s.Sessions(t.Context(), current, "", 30)
			if !errors.Is(err, ErrUnauthenticated) || len(items) != 0 {
				t.Fatalf("expired caller returned %d sessions, error=%v", len(items), err)
			}
			var revoked, audit, changes, outbox int
			if err := s.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions WHERE id=? AND revoked_at IS NOT NULL),(SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired' AND resource_id=? AND json_extract(detail_json,'$.reason')=?),(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete' AND c.resource_id=?)`, current.ID, current.ID, kind, current.ID, current.ID).Scan(&revoked, &audit, &changes, &outbox); err != nil || revoked != 1 || audit != 1 || changes != 1 || outbox != 1 {
				t.Fatalf("expiry journal: revoked=%d audit=%d changes=%d outbox=%d error=%v", revoked, audit, changes, outbox, err)
			}
		})
	}
}

func TestSessionsExpiryPersistenceFailureDoesNotReturnData(t *testing.T) {
	s, _ := testService(t)
	clock := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	current := login(t, s)
	clock = clock.Add(s.policy.Idle)
	if _, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER fail_sessions_expiry BEFORE INSERT ON audit_events WHEN NEW.event_type='session_expired' BEGIN SELECT RAISE(ABORT,'test audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	items, err := s.Sessions(t.Context(), current, "", 30)
	if err == nil || errors.Is(err, ErrUnauthenticated) || len(items) != 0 {
		t.Fatalf("expiry persistence failure returned %d sessions, error=%v", len(items), err)
	}
	var revoked sql.NullInt64
	var changes int
	if err := s.db.QueryRowContext(t.Context(), `SELECT revoked_at,(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?) FROM sessions WHERE id=?`, current.ID, current.ID).Scan(&revoked, &changes); err != nil || revoked.Valid || changes != 0 {
		t.Fatalf("failed expiry left partial state: revoked=%v changes=%d error=%v", revoked, changes, err)
	}
	if _, err := s.db.ExecContext(t.Context(), `DROP TRIGGER fail_sessions_expiry`); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(-time.Millisecond)
	if _, err := s.Authenticate(t.Context(), current.Token); err != nil {
		t.Fatalf("failed persistence incorrectly claimed durable expiry: %v", err)
	}
}

func TestSessionsReadUsesOneCommittedSnapshot(t *testing.T) {
	s, _ := testService(t)
	current := login(t, s)
	login(t, s)
	writer, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(t.Context(), `UPDATE sessions SET revoked_at=? WHERE id=?`, time.Now().UnixMilli(), current.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	items, err := s.Sessions(ctx, current, "", 30)
	if err != nil || len(items) != 2 {
		t.Fatalf("reader did not see the prior committed snapshot: count=%d error=%v", len(items), err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	items, err = s.Sessions(t.Context(), current, "", 30)
	if !errors.Is(err, ErrUnauthenticated) || len(items) != 0 {
		t.Fatalf("committed revocation returned %d sessions, error=%v", len(items), err)
	}
}

func TestSessionsReadOnlyTransactionAllowsConcurrentWriter(t *testing.T) {
	s, _ := testService(t)
	current := login(t, s)
	writer, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := s.SessionInTx(t.Context(), writer, current); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET user_agent='changed' WHERE id=?`, current.ID); err != nil {
		t.Fatalf("read transaction blocked a separate writer: %v", err)
	}
}
