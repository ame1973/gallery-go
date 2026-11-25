package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gallerygo/internal/store"
)

func TestLoginReplacementJournalsPreviousSession(t *testing.T) {
	for _, target := range []string{"same-account", "other-account"} {
		t.Run(target, func(t *testing.T) {
			s, _ := testService(t)
			old := login(t, s)
			username := old.User.Username
			if target == "other-account" {
				username = managedUser(t, s, old, "reader", "user").Username
			}
			current, err := s.Login(t.Context(), username, strongPassword, "192.0.2.12", "rotation test", old.Token)
			if err != nil {
				t.Fatal(err)
			}
			if current.ID == old.ID || current.Token == old.Token {
				t.Fatal("login did not rotate the session")
			}
			if _, err := s.Authenticate(t.Context(), old.Token); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("previous session still authenticates: %v", err)
			}
			if _, err := s.Authenticate(t.Context(), current.Token); err != nil {
				t.Fatalf("replacement session is not usable: %v", err)
			}

			var seq int64
			var scope, payload string
			if err := s.db.QueryRowContext(t.Context(), `SELECT seq,actor_scope,payload_json FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=? AND version=1`, old.ID).Scan(&seq, &scope, &payload); err != nil {
				t.Fatalf("missing previous-session delete event: %v", err)
			}
			var event map[string]string
			if err := json.Unmarshal([]byte(payload), &event); err != nil || len(event) != 1 || event["user_id"] != old.User.ID || scope != old.User.ScopeID {
				t.Fatalf("incorrect previous-session event: scope=%s event=%v error=%v", scope, event, err)
			}
			var outbox int
			if err := s.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM outbox_events WHERE change_seq=?`, seq).Scan(&outbox); err != nil || outbox != 1 {
				t.Fatalf("previous-session outbox count=%d error=%v", outbox, err)
			}
			var actor, detail string
			if err := s.db.QueryRowContext(t.Context(), `SELECT actor_user_id,detail_json FROM audit_events WHERE event_type='login_session_replaced' AND resource_type='session' AND resource_id=?`, old.ID).Scan(&actor, &detail); err != nil {
				t.Fatalf("missing previous-session audit: %v", err)
			}
			var audit map[string]string
			if err := json.Unmarshal([]byte(detail), &audit); err != nil || actor != current.User.ID || len(audit) != 1 || audit["user_id"] != old.User.ID {
				t.Fatalf("incorrect previous-session audit: actor=%s detail=%v error=%v", actor, audit, err)
			}
			for _, secret := range []string{old.Token, old.CSRFToken(), digest(old.Token), current.Token, current.CSRFToken(), digest(current.Token), strongPassword} {
				if strings.Contains(payload, secret) || strings.Contains(detail, secret) {
					t.Fatal("previous-session journal contains a credential")
				}
			}

			// Reusing a revoked token must not journal or revoke it twice.
			third, err := s.Login(t.Context(), username, strongPassword, "192.0.2.12", "rotation test", old.Token)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Authenticate(t.Context(), current.Token); err != nil {
				t.Fatalf("stale token revoked the replacement: %v", err)
			}
			if _, err := s.Authenticate(t.Context(), third.Token); err != nil {
				t.Fatal(err)
			}
			var changes, audits int
			if err := s.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?),(SELECT COUNT(*) FROM audit_events WHERE event_type='login_session_replaced' AND resource_id=?)`, old.ID, old.ID).Scan(&changes, &audits); err != nil || changes != 1 || audits != 1 {
				t.Fatalf("repeated replacement journals: changes=%d audits=%d error=%v", changes, audits, err)
			}
		})
	}
}

func TestLoginReplacementRecordsExpiredAndIgnoresMissingTokens(t *testing.T) {
	s, _ := testService(t)
	old := login(t, s)
	if _, err := s.db.ExecContext(t.Context(), `UPDATE sessions SET expires_at=created_at WHERE id=?`, old.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.Login(t.Context(), old.User.Username, strongPassword, "192.0.2.12", "rotation test", old.Token)
	if err != nil {
		t.Fatal(err)
	}
	var revoked sql.NullInt64
	var changes, outbox, audits int
	if err := s.db.QueryRowContext(t.Context(), `SELECT revoked_at,(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=? AND actor_scope=?),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete' AND c.resource_id=?),(SELECT COUNT(*) FROM audit_events WHERE event_type='login_session_replaced' AND resource_id=?) FROM sessions WHERE id=?`, old.ID, old.User.ScopeID, old.ID, old.ID, old.ID).Scan(&revoked, &changes, &outbox, &audits); err != nil {
		t.Fatal(err)
	}
	if !revoked.Valid || changes != 1 || outbox != 1 || audits != 1 {
		t.Fatalf("unrevoked expired session: revoked=%v changes=%d outbox=%d audits=%d", revoked, changes, outbox, audits)
	}
	for _, previous := range []string{"invalid-token", token()} {
		if _, err := s.Login(t.Context(), old.User.Username, strongPassword, "192.0.2.12", "rotation test", previous); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Authenticate(t.Context(), current.Token); err != nil {
		t.Fatalf("an unrelated token revoked the current session: %v", err)
	}
	if err := s.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete'),(SELECT COUNT(*) FROM audit_events WHERE event_type='login_session_replaced')`).Scan(&changes, &audits); err != nil || changes != 1 || audits != 1 {
		t.Fatalf("missing tokens created spurious journals: changes=%d audits=%d error=%v", changes, audits, err)
	}
}

func TestConcurrentLoginReplacementJournalsOnce(t *testing.T) {
	s, _ := testService(t)
	old := login(t, s)
	var sequence int
	var name, path string
	if err := s.db.QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	otherDB, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	other, err := New(otherDB, s.policy)
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		session Session
		err     error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for i, service := range []*Service{s, other} {
		go func() {
			<-start
			ip := []string{"192.0.2.12", "192.0.2.13"}[i]
			session, err := service.Login(t.Context(), old.User.Username, strongPassword, ip, "rotation test", old.Token)
			results <- outcome{session, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("concurrent logins: first=%v second=%v", first.err, second.err)
	}
	if first.session.ID == second.session.ID || first.session.Token == second.session.Token {
		t.Fatal("concurrent logins reused a session")
	}
	for _, session := range []Session{first.session, second.session} {
		if _, err := s.Authenticate(t.Context(), session.Token); err != nil {
			t.Fatalf("stale input revoked a later session: %v", err)
		}
	}
	var changes, outbox, audits int
	if err := s.db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete' AND c.resource_id=?),(SELECT COUNT(*) FROM audit_events WHERE event_type='login_session_replaced' AND resource_id=?)`, old.ID, old.ID, old.ID).Scan(&changes, &outbox, &audits); err != nil || changes != 1 || outbox != 1 || audits != 1 {
		t.Fatalf("concurrent journals: changes=%d outbox=%d audits=%d error=%v", changes, outbox, audits, err)
	}
}

func TestLoginReplacementUsesPreviousAccountCurrentScope(t *testing.T) {
	s, _ := testService(t)
	old := login(t, s)
	reader := managedUser(t, s, old, "reader", "user")
	freshScope := token()
	if _, err := s.db.ExecContext(t.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, freshScope, old.User.ID); err != nil {
		t.Fatal(err)
	}
	freshOld, err := s.Authenticate(t.Context(), old.Token)
	if err != nil || freshOld.User.ScopeID != freshScope || freshOld.User.ScopeID == old.User.ScopeID {
		t.Fatalf("old session did not retain its changed current scope: %v", err)
	}
	current, err := s.Login(t.Context(), reader.Username, strongPassword, "192.0.2.12", "rotation test", old.Token)
	if err != nil {
		t.Fatal(err)
	}
	var scope, actor string
	if err := s.db.QueryRowContext(t.Context(), `SELECT actor_scope FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?`, old.ID).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(t.Context(), `SELECT actor_user_id FROM audit_events WHERE event_type='login_session_replaced' AND resource_id=?`, old.ID).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	if scope != freshScope || actor != current.User.ID || actor != reader.ID {
		t.Fatalf("previous session used stale scope or wrong actor: scope=%s actor=%s", scope, actor)
	}
}

func TestLoginReplacementJournalFailureRollsBack(t *testing.T) {
	for _, stage := range []struct {
		name, trigger string
	}{
		{"change", `CREATE TRIGGER fail_login_rotation BEFORE INSERT ON change_log WHEN NEW.kind='session' AND NEW.operation='delete' BEGIN SELECT RAISE(ABORT,'test change failure'); END`},
		{"outbox", `CREATE TRIGGER fail_login_rotation BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log WHERE seq=NEW.change_seq AND kind='session' AND operation='delete') BEGIN SELECT RAISE(ABORT,'test outbox failure'); END`},
		{"rotation-audit", `CREATE TRIGGER fail_login_rotation BEFORE INSERT ON audit_events WHEN NEW.event_type='login_session_replaced' BEGIN SELECT RAISE(ABORT,'test rotation audit failure'); END`},
		{"new-change", `CREATE TRIGGER fail_login_rotation BEFORE INSERT ON change_log WHEN NEW.kind='session' AND NEW.operation='upsert' BEGIN SELECT RAISE(ABORT,'test new session change failure'); END`},
		{"new-outbox", `CREATE TRIGGER fail_login_rotation BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log WHERE seq=NEW.change_seq AND kind='session' AND operation='upsert') BEGIN SELECT RAISE(ABORT,'test new session outbox failure'); END`},
		{"login-audit", `CREATE TRIGGER fail_login_rotation BEFORE INSERT ON audit_events WHEN NEW.event_type='login_succeeded' BEGIN SELECT RAISE(ABORT,'test login audit failure'); END`},
	} {
		t.Run(stage.name, func(t *testing.T) {
			s, _ := testService(t)
			old := login(t, s)
			var before, after [4]int
			query := `SELECT (SELECT COUNT(*) FROM sessions),(SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`
			if err := s.db.QueryRowContext(t.Context(), query).Scan(&before[0], &before[1], &before[2], &before[3]); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(t.Context(), stage.trigger); err != nil {
				t.Fatal(err)
			}
			_, err := s.Login(t.Context(), old.User.Username, strongPassword, "192.0.2.12", "rotation test", old.Token)
			if _, dropErr := s.db.ExecContext(t.Context(), `DROP TRIGGER fail_login_rotation`); dropErr != nil {
				t.Fatal(dropErr)
			}
			if err == nil {
				t.Error("login succeeded despite injected journal failure")
			}
			if err := s.db.QueryRowContext(t.Context(), query).Scan(&after[0], &after[1], &after[2], &after[3]); err != nil {
				t.Fatal(err)
			}
			var revoked sql.NullInt64
			if err := s.db.QueryRowContext(t.Context(), `SELECT revoked_at FROM sessions WHERE id=?`, old.ID).Scan(&revoked); err != nil {
				t.Fatal(err)
			}
			if before != after || revoked.Valid {
				t.Errorf("journal failure left partial state: before=%v after=%v revoked=%v", before, after, revoked)
			}
			if _, err := s.Authenticate(t.Context(), old.Token); err != nil {
				t.Errorf("journal failure invalidated the previous session: %v", err)
			}
		})
	}
}
