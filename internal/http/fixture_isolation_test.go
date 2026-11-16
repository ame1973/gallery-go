package httpapi

import "testing"

func TestHTTPFixtureDatabasesAndSessionsAreIsolated(t *testing.T) {
	firstDB, first := authFixture(t)
	secondDB, second := authFixture(t)
	if firstDB == secondDB {
		t.Fatal("fixtures share a database handle")
	}
	firstCookie, firstCSRF, firstUser := loginHTTP(t, first)
	secondCookie, _, secondUser := loginHTTP(t, second)
	// The image deliberately shares a test-only owner, not any session.
	if firstUser.ID != secondUser.ID || firstUser.ScopeID != secondUser.ScopeID {
		t.Fatal("fixtures did not use the expected bootstrap image")
	}
	if firstCookie.Value == secondCookie.Value {
		t.Fatal("separate fixture logins shared a cookie")
	}
	if status := requestAuth(first, "GET", "/api/v2/auth/me", "", "", "", secondCookie).Code; status != 401 {
		t.Fatal("first fixture accepted the second cookie", status)
	}
	if status := requestAuth(second, "GET", "/api/v2/auth/me", "", "", "", firstCookie).Code; status != 401 {
		t.Fatal("second fixture accepted the first cookie", status)
	}
	if status := requestAuth(first, "POST", "/api/v2/auth/logout", "", testOrigin, firstCSRF, firstCookie).Code; status != 204 {
		t.Fatal("first fixture logout failed", status)
	}
	if status := requestAuth(second, "GET", "/api/v2/auth/me", "", "", "", secondCookie).Code; status != 200 {
		t.Fatal("first fixture logout changed the second session", status)
	}
}
