package httpapi

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestAuthHTTPObservedExpiryPersists(t *testing.T) {
	for _, kind := range []string{"idle", "absolute"} {
		t.Run(kind, func(t *testing.T) {
			db, h := authFixture(t)
			cookie, csrf, _ := loginHTTP(t, h)
			var id string
			if err := db.QueryRowContext(t.Context(), `SELECT id FROM sessions`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			// Seed only this disposable fixture's timestamps; this is not a wall-clock test.
			now := time.Now().UTC()
			statement := `UPDATE sessions SET created_at=?,last_seen_at=? WHERE id=?`
			first, second := now.Add(-48*time.Hour), now.Add(-25*time.Hour)
			if kind == "absolute" {
				statement = `UPDATE sessions SET created_at=?,expires_at=? WHERE id=?`
				first, second = now.Add(-31*24*time.Hour), now.Add(-time.Hour)
			}
			if _, err := db.ExecContext(t.Context(), statement, first.UnixMilli(), second.UnixMilli(), id); err != nil {
				t.Fatal(err)
			}
			for _, request := range []struct{ method, path string }{
				{"GET", "/api/v2/auth/me"},
				{"GET", "/api/v2/albums"},
				{"POST", "/api/v2/auth/logout"},
			} {
				w := requestAuth(h, request.method, request.path, "", testOrigin, csrf, cookie)
				expectCode(t, w, 401, "UNAUTHENTICATED")
				if len(w.Result().Cookies()) != 0 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
					t.Fatal("expiry changed a cookie or allowed caching")
				}
				for _, secret := range []string{cookie.Value, csrf, testPassword} {
					if strings.Contains(problemContent(t, w), secret) {
						t.Fatal("expiry problem leaked a secret")
					}
				}
			}
			var revoked sql.NullInt64
			var changes, outbox, audit int
			if err := db.QueryRowContext(t.Context(), `SELECT revoked_at,(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete' AND c.resource_id=?),(SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired' AND resource_id=?) FROM sessions WHERE id=?`, id, id, id, id).Scan(&revoked, &changes, &outbox, &audit); err != nil {
				t.Fatal(err)
			}
			if !revoked.Valid || changes != 1 || outbox != 1 || audit != 1 {
				t.Fatalf("expiry not journaled exactly once: revoked=%v changes=%d outbox=%d audit=%d", revoked, changes, outbox, audit)
			}
			var detail string
			if err := db.QueryRowContext(t.Context(), `SELECT detail_json FROM audit_events WHERE event_type='session_expired' AND resource_id=?`, id).Scan(&detail); err != nil {
				t.Fatal(err)
			}
			if detail != `{"reason":"`+kind+`"}` {
				t.Fatalf("unexpected expiry reason: %s", detail)
			}
			fresh, _, _ := loginHTTP(t, h)
			if w := requestAuth(h, "GET", "/api/v2/auth/me", "", "", "", fresh); w.Code != 200 {
				t.Fatalf("expiry blocked a new independent session: %d", w.Code)
			}
		})
	}
}

func TestAuthHTTPExpiryPersistenceFailureIsPrivate(t *testing.T) {
	db, h := authFixture(t)
	cookie, csrf, _ := loginHTTP(t, h)
	if _, err := db.ExecContext(t.Context(), `UPDATE sessions SET last_seen_at=?`, time.Now().Add(-25*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER fail_expiry BEFORE INSERT ON audit_events WHEN NEW.event_type='session_expired' BEGIN SELECT RAISE(ABORT,'private-test-expiry SQL /private/test-fixture'); END`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v2/auth/me", "/api/v2/albums"} {
		w := requestAuth(h, "GET", path, "", "", "", cookie)
		expectCode(t, w, 500, "INTERNAL_ERROR")
		if len(w.Result().Cookies()) != 0 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("failed expiry changed a cookie or allowed caching")
		}
		for _, secret := range []string{"private-test-expiry", "SQL", "/private/test-fixture", cookie.Value, csrf, testPassword} {
			if strings.Contains(problemContent(t, w), secret) {
				t.Fatal("expiry persistence error leaked diagnostics")
			}
		}
	}
	var revoked, changes, audit int
	if err := db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions WHERE revoked_at IS NOT NULL),(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete'),(SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired')`).Scan(&revoked, &changes, &audit); err != nil {
		t.Fatal(err)
	}
	if revoked != 0 || changes != 0 || audit != 0 {
		t.Fatalf("failure left partial expiry: revoked=%d changes=%d audit=%d", revoked, changes, audit)
	}
	if _, err := db.ExecContext(t.Context(), `DROP TRIGGER fail_expiry`); err != nil {
		t.Fatal(err)
	}
	w := requestAuth(h, "GET", "/api/v2/auth/me", "", "", "", cookie)
	expectCode(t, w, 401, "UNAUTHENTICATED")
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE revoked_at IS NOT NULL`).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("fresh observation after injected failure: revoked=%d error=%v", revoked, err)
	}
}
