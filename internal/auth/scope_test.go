package auth

import "testing"

func TestPasswordChangeScopeRevocationRollsBackWithAccountEvent(t *testing.T) {
	s, user := testService(t)
	first, second := login(t, s), login(t, s)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_user_event BEFORE INSERT ON change_log WHEN NEW.kind='user' BEGIN SELECT RAISE(ABORT,'injected account event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangePassword(t.Context(), first, strongPassword, "a replacement that must not be committed"); err == nil {
		t.Fatal("password change committed without its account event")
	}
	for _, before := range []Session{first, second} {
		after, err := s.Authenticate(t.Context(), before.Token)
		if err != nil || after.User != user || !after.ValidCSRF(before.CSRFToken()) {
			t.Fatalf("partially rotated account/session: %+v %v", after, err)
		}
	}
	var revokes, orphanEvents int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log WHERE kind='scope'),(SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE o.id IS NULL)`).Scan(&revokes, &orphanEvents); err != nil || revokes != 0 || orphanEvents != 0 {
		t.Fatalf("partial scope/outbox event: revokes=%d orphans=%d %v", revokes, orphanEvents, err)
	}
	if _, err := s.Login(t.Context(), user.Username, strongPassword, "192.0.2.1", "after rollback", ""); err != nil {
		t.Fatal("old password was not restored by rollback", err)
	}
}
