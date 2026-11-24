package auth

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/store"
)

func TestObservedExpiryIsDurableAfterReopen(t *testing.T) {
	for _, kind := range []string{"idle", "absolute"} {
		for _, entry := range []string{"authenticate", "transaction"} {
			t.Run(kind+"/"+entry, func(t *testing.T) {
				s, _ := testService(t)
				clock := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
				s.now = func() time.Time { return clock }
				session := login(t, s)
				expires := clock.Add(s.policy.Idle)
				if kind == "absolute" {
					expires = session.ExpiresAt
					for next := clock.Add(12 * time.Hour); next.Before(expires); next = next.Add(12 * time.Hour) {
						clock = next
						if _, err := s.Authenticate(t.Context(), session.Token); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, err := s.db.ExecContext(t.Context(), `CREATE TABLE expiry_business(value INTEGER)`); err != nil {
					t.Fatal(err)
				}
				clock = expires
				var rejection error
				if entry == "authenticate" {
					_, rejection = s.Authenticate(t.Context(), session.Token)
				} else {
					tx, err := s.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO expiry_business VALUES(1)`); err != nil {
						t.Fatal(err)
					}
					_, rejection = s.SessionInTx(t.Context(), tx, session)
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO expiry_business VALUES(2)`); !errors.Is(err, sql.ErrTxDone) {
						t.Errorf("expired transaction remained usable: %v", err)
					}
					if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
						t.Fatal(err)
					}
				}
				if !errors.Is(rejection, ErrUnauthenticated) {
					t.Fatalf("expiry rejection: %v", rejection)
				}
				var sequence int
				var databaseName, path string
				if err := s.db.QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&sequence, &databaseName, &path); err != nil {
					t.Fatal(err)
				}
				if err := s.db.Close(); err != nil {
					t.Fatal(err)
				}
				db, err := store.Open(t.Context(), path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				fresh, err := New(db, s.policy)
				if err != nil {
					t.Fatal(err)
				}
				clock = expires.Add(-time.Millisecond)
				fresh.now = func() time.Time { return clock }
				if _, err := fresh.Authenticate(t.Context(), session.Token); !errors.Is(err, ErrUnauthenticated) {
					t.Errorf("reopened service accepted expired session after rollback: %v", err)
				}
				var revoked sql.NullInt64
				var created, last int64
				if err := db.QueryRowContext(t.Context(), `SELECT revoked_at,created_at,last_seen_at FROM sessions WHERE id=?`, session.ID).Scan(&revoked, &created, &last); err != nil {
					t.Fatal(err)
				}
				if !revoked.Valid || revoked.Int64 != expires.UnixMilli() || created > clock.UnixMilli() || last > clock.UnixMilli() {
					t.Errorf("revocation/timestamp mismatch: revoked=%v created=%d last=%d", revoked, created, last)
				}
				var business, audit, changes, outbox int
				if err := db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM expiry_business),(SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired' AND resource_id=?),(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete' AND c.resource_id=?)`, session.ID, session.ID, session.ID).Scan(&business, &audit, &changes, &outbox); err != nil {
					t.Fatal(err)
				}
				if business != 0 || audit != 1 || changes != 1 || outbox != 1 {
					t.Errorf("expiry atomicity: business=%d audit=%d changes=%d outbox=%d", business, audit, changes, outbox)
				}
			})
		}
	}
}

func TestObservedExpiryAuditFailureDoesNotClaimRevocation(t *testing.T) {
	s, _ := testService(t)
	clock := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	session := login(t, s)
	clock = clock.Add(s.policy.Idle)
	if _, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER fail_expiry BEFORE INSERT ON audit_events WHEN NEW.event_type='session_expired' BEGIN SELECT RAISE(ABORT,'test expiry audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), session.Token); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("persistence failure claimed a confirmed expiry: %v", err)
	}
	var revoked sql.NullInt64
	var changes int
	if err := s.db.QueryRowContext(t.Context(), `SELECT revoked_at,(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?) FROM sessions WHERE id=?`, session.ID, session.ID).Scan(&revoked, &changes); err != nil {
		t.Fatal(err)
	}
	if revoked.Valid || changes != 0 {
		t.Fatalf("late failure left partial revocation: %v changes=%d", revoked, changes)
	}
	if _, err := s.db.ExecContext(t.Context(), `DROP TRIGGER fail_expiry`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expiry after removing injected failure: %v", err)
	}
}

func TestObservedExpiryCommitFailureDiscardsUncommittedState(t *testing.T) {
	s, _ := testService(t)
	clock := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	session := login(t, s)
	clock = clock.Add(s.policy.Idle)
	// A deferred foreign key fails at COMMIT, after every mutation statement ran.
	// Reusing the sole connection must not expose a failed transaction's marker.
	s.db.SetMaxOpenConns(1)
	for _, statement := range []string{
		`CREATE TABLE expiry_parent(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE expiry_child(parent_id INTEGER REFERENCES expiry_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER fail_expiry_commit AFTER INSERT ON audit_events WHEN NEW.event_type='session_expired' BEGIN INSERT INTO expiry_child VALUES(1); END`,
	} {
		if _, err := s.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Authenticate(t.Context(), session.Token); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("commit failure claimed confirmed expiry: %v", err)
	}
	var revoked sql.NullInt64
	var changes, audit, outbox, child int
	if err := s.db.QueryRowContext(t.Context(), `SELECT revoked_at,(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?),(SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired'),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete' AND c.resource_id=?),(SELECT COUNT(*) FROM expiry_child) FROM sessions WHERE id=?`, session.ID, session.ID, session.ID).Scan(&revoked, &changes, &audit, &outbox, &child); err != nil {
		t.Fatal(err)
	}
	if revoked.Valid || changes != 0 || audit != 0 || outbox != 0 || child != 0 {
		t.Errorf("failed commit left visible state: revoked=%v changes=%d audit=%d outbox=%d child=%d", revoked, changes, audit, outbox, child)
	}
	if _, err := s.db.ExecContext(t.Context(), `DROP TRIGGER fail_expiry_commit`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), session.Token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("fresh expiry after commit failure: %v", err)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("failed commit poisoned pooled connection: %v", err)
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("committed expiry count=%d error=%v", count, err)
	}
}

func TestObservedExpiryJournalFailureAbortsCaller(t *testing.T) {
	for _, stage := range []struct {
		name, trigger string
	}{
		{"change", `CREATE TRIGGER fail_expiry BEFORE INSERT ON change_log WHEN NEW.kind='session' AND NEW.operation='delete' BEGIN SELECT RAISE(ABORT,'test expiry change failure'); END`},
		{"outbox", `CREATE TRIGGER fail_expiry BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log WHERE seq=NEW.change_seq AND kind='session' AND operation='delete') BEGIN SELECT RAISE(ABORT,'test expiry outbox failure'); END`},
		{"audit", `CREATE TRIGGER fail_expiry BEFORE INSERT ON audit_events WHEN NEW.event_type='session_expired' BEGIN SELECT RAISE(ABORT,'test expiry audit failure'); END`},
	} {
		t.Run(stage.name, func(t *testing.T) {
			s, _ := testService(t)
			clock := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
			s.now = func() time.Time { return clock }
			session := login(t, s)
			for _, statement := range []string{`CREATE TABLE expiry_business(value INTEGER)`, stage.trigger} {
				if _, err := s.db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			var before [3]int
			if err := s.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`).Scan(&before[0], &before[1], &before[2]); err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := s.RequireAdmin(t.Context(), tx, session); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO expiry_business VALUES(1)`); err != nil {
				t.Fatal(err)
			}
			if err := store.Change(t.Context(), tx, session.User.ScopeID, "expiry_test", session.ID, "upsert", 1, nil, clock); err != nil {
				t.Fatal(err)
			}
			clock = clock.Add(s.policy.Idle)
			if _, err := s.RequireAdmin(t.Context(), tx, session); err == nil || errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("journal failure claimed confirmed expiry: %v", err)
			}
			if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("expired caller transaction remained committable: %v", err)
			}
			var after [3]int
			var business int
			var revoked sql.NullInt64
			if err := s.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events),(SELECT COUNT(*) FROM expiry_business),revoked_at FROM sessions WHERE id=?`, session.ID).Scan(&after[0], &after[1], &after[2], &business, &revoked); err != nil {
				t.Fatal(err)
			}
			if before != after || business != 0 || revoked.Valid {
				t.Fatalf("partial expiry/business journal: before=%v after=%v business=%d revoked=%v", before, after, business, revoked)
			}
			if _, err := s.db.ExecContext(t.Context(), `DROP TRIGGER fail_expiry`); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Authenticate(t.Context(), session.Token); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("fresh observation after removing injected failure: %v", err)
			}
		})
	}
}

func TestObservedExpiryDoesNotRevokeNewerActivity(t *testing.T) {
	s, _ := testService(t)
	clock := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	session := login(t, s)
	expires := clock.Add(s.policy.Idle)
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var seen int64
	if err := tx.QueryRowContext(t.Context(), `SELECT last_seen_at FROM sessions WHERE id=?`, session.ID).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	other, err := New(s.db, s.policy)
	if err != nil {
		t.Fatal(err)
	}
	other.now = func() time.Time { return expires.Add(-time.Millisecond) }
	if _, err := other.Authenticate(t.Context(), session.Token); err != nil {
		t.Fatalf("newer valid activity: %v", err)
	}
	clock = expires
	if _, err := s.SessionInTx(t.Context(), tx, session); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale expiry must report a conflict, not confirmed expiry: %v", err)
	}
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(t.Context(), session.Token); err != nil {
		t.Fatalf("newer activity was revoked: %v", err)
	}
	var count int
	if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected expiry audit: count=%d error=%v", count, err)
	}
}

func TestObservedExpiryCancellationWhileWriterHeld(t *testing.T) {
	s, _ := testService(t)
	base := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	session := login(t, s)
	writer, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- s.confirmExpiry(ctx, session.Token, base.Add(s.policy.Idle)) }()
	// The second borrowed connection cannot acquire the writer while it is held.
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for s.db.Stats().InUse < 2 {
		select {
		case err := <-result:
			t.Fatalf("expiry returned before writer release: %v", err)
		case <-deadline.C:
			t.Fatal("expiry did not reach the held writer")
		case <-poll.C:
		}
	}
	cancel()
	if err := writer.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled confirmation: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cancelled expiry did not return after writer release")
	}
	var revoked, changes, audit int
	if err := s.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions WHERE revoked_at IS NOT NULL),(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete'),(SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired')`).Scan(&revoked, &changes, &audit); err != nil || revoked != 0 || changes != 0 || audit != 0 {
		t.Fatalf("cancelled confirmation persisted state: revoked=%d changes=%d audit=%d error=%v", revoked, changes, audit, err)
	}
	if err := s.confirmExpiry(t.Context(), session.Token, base.Add(s.policy.Idle)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("fresh confirmation after cancellation: %v", err)
	}
}

func TestObservedExpiryConcurrentObserversJournalOnce(t *testing.T) {
	s, _ := testService(t)
	base := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }
	session := login(t, s)
	s.now = func() time.Time { return base.Add(s.policy.Idle) }
	var sequence int
	var databaseName, path string
	if err := s.db.QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&sequence, &databaseName, &path); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	other, err := New(db, s.policy)
	if err != nil {
		t.Fatal(err)
	}
	other.now = s.now
	var group sync.WaitGroup
	for i := range 16 {
		group.Go(func() {
			observer := s
			if i%2 != 0 {
				observer = other
			}
			var err error
			if i%4 < 2 {
				_, err = observer.Authenticate(t.Context(), session.Token)
			} else {
				var tx *sql.Tx
				tx, err = observer.db.BeginTx(t.Context(), nil)
				if err == nil {
					defer tx.Rollback()
					_, err = observer.SessionInTx(t.Context(), tx, session)
				}
			}
			if !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("concurrent expiry returned %v", err)
			}
		})
	}
	group.Wait()
	var audit, changes, outbox int
	if err := s.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired' AND resource_id=?),(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete' AND c.resource_id=?)`, session.ID, session.ID, session.ID).Scan(&audit, &changes, &outbox); err != nil || audit != 1 || changes != 1 || outbox != 1 {
		t.Fatalf("expiry journal audit=%d changes=%d outbox=%d error=%v", audit, changes, outbox, err)
	}
}
