package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestLoginReplacementJournalFailureKeepsPreviousCookie(t *testing.T) {
	db, handler := authFixture(t)
	old, _, _ := loginHTTP(t, handler)
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER fail_login_rotation_http BEFORE INSERT ON audit_events WHEN NEW.event_type='login_session_replaced' BEGIN SELECT RAISE(ABORT,'test audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(loginRequest{Username: "owner", Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}
	response := requestAuth(handler, http.MethodPost, "/api/v2/auth/login", string(body), testOrigin, "", old)
	assertProblem(t, response, http.StatusInternalServerError)
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("failed login sent a session cookie")
	}
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER fail_login_rotation_http`); err != nil {
		t.Fatal(err)
	}
	me := requestAuth(handler, http.MethodGet, "/api/v2/auth/me", "", "", "", old)
	if me.Code != http.StatusOK {
		t.Fatalf("failed login invalidated previous session: %d", me.Code)
	}
	var sessions, changes, outbox, audits int
	if err := db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions),(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete'),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete'),(SELECT COUNT(*) FROM audit_events WHERE event_type='login_session_replaced')`).Scan(&sessions, &changes, &outbox, &audits); err != nil || sessions != 1 || changes != 0 || outbox != 0 || audits != 0 {
		t.Fatalf("failed login left partial state: sessions=%d changes=%d outbox=%d audits=%d error=%v", sessions, changes, outbox, audits, err)
	}
}
