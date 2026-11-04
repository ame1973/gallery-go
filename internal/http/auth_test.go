package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/auth"
)

const testOrigin = "https://gallery.test"
const testPassword = "a safely chosen gallery passphrase"

func authFixture(t *testing.T) (*sql.DB, http.Handler) {
	t.Helper()
	db, h := newTestServer(t)
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Bootstrap(t.Context(), "owner", testPassword); err != nil {
		t.Fatal(err)
	}
	return db, h
}

func requestAuth(h http.Handler, method, path, body, origin, csrf string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.4:12345"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func loginHTTP(t *testing.T, h http.Handler) (*http.Cookie, string, auth.User) {
	t.Helper()
	data, _ := json.Marshal(loginRequest{Username: "owner", Password: testPassword})
	w := requestAuth(h, "POST", "/api/v2/auth/login", string(data), testOrigin, "", nil)
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: %+v", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != sessionCookie || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" || cookie.MaxAge <= 0 {
		t.Fatalf("unsafe cookie: %s", cookie)
	}
	var user auth.User
	if err := json.Unmarshal(w.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	w = requestAuth(h, "GET", "/api/v2/auth/csrf", "", "", "", cookie)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var csrf csrfResponse
	if err := json.Unmarshal(w.Body.Bytes(), &csrf); err != nil {
		t.Fatal(err)
	}
	return cookie, csrf.Token, user
}

func TestAuthHTTPLoginMeCSRFLogout(t *testing.T) {
	_, h := authFixture(t)
	cookie, csrf, user := loginHTTP(t, h)
	w := requestAuth(h, "GET", "/api/v2/auth/me?user_id=another", "", "", "", cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), user.ID) || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("me: %d %s", w.Code, w.Body.String())
	}
	w = requestAuth(h, "POST", "/api/v2/auth/logout", "", testOrigin, csrf, cookie)
	if w.Code != 204 || w.Body.Len() != 0 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	w = requestAuth(h, "GET", "/api/v2/auth/me", "", "", "", cookie)
	if w.Code != 401 {
		t.Fatalf("revoked cookie accepted: %d", w.Code)
	}
	assertProblem(t, w, 401)
}

func TestAuthHTTPRejectsAnonymousForgedAndAmbiguousCookies(t *testing.T) {
	_, h := authFixture(t)
	for _, op := range Operations {
		if op.Public {
			continue
		}
		for _, cookie := range []*http.Cookie{nil, {Name: sessionCookie, Value: "1"}, {Name: sessionCookie, Value: "forged"}} {
			w := requestAuth(h, op.Method, "/api/v2"+op.Path, "", testOrigin, "invalid", cookie)
			if w.Code != 401 {
				t.Errorf("%s anonymous: %d", op.ID, w.Code)
			}
		}
	}
	cookie, _, _ := loginHTTP(t, h)
	r := httptest.NewRequest("GET", "/api/v2/auth/me", nil)
	r.AddCookie(cookie)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("ambiguous duplicate session cookies accepted")
	}
}

func TestAuthHTTPOriginAndCSRFMatrix(t *testing.T) {
	_, h := authFixture(t)
	body, _ := json.Marshal(loginRequest{Username: "owner", Password: testPassword})
	for _, origin := range []string{"", "null", "http://gallery.test", "https://evil.test", "https://gallery.test.evil.test"} {
		w := requestAuth(h, "POST", "/api/v2/auth/login", string(body), origin, "", nil)
		if w.Code != 403 {
			t.Errorf("origin %q: %d", origin, w.Code)
		}
		if len(w.Result().Cookies()) != 0 {
			t.Error("cookie issued on rejected origin")
		}
	}
	cookie, csrf, _ := loginHTTP(t, h)
	for _, c := range []struct{ origin, csrf string }{{testOrigin, ""}, {testOrigin, "wrong"}, {"", csrf}, {"https://evil.test", csrf}} {
		w := requestAuth(h, "POST", "/api/v2/auth/logout", "", c.origin, c.csrf, cookie)
		if w.Code != 403 {
			t.Fatalf("CSRF case %+v: %d", c, w.Code)
		}
	}
	other, otherCSRF, _ := loginHTTP(t, h)
	w := requestAuth(h, "POST", "/api/v2/auth/logout", "", testOrigin, otherCSRF, cookie)
	if w.Code != 403 {
		t.Fatal("cross-session CSRF accepted")
	}
	r := httptest.NewRequest("POST", "/api/v2/auth/logout", nil)
	r.AddCookie(other)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("X-CSRF-Token", otherCSRF)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-site fetch accepted")
	}
	w = requestAuth(h, "GET", "/api/v2/auth/me", "", "", "", cookie)
	if w.Code != 200 {
		t.Fatal("rejected CSRF changed session state")
	}
}

func TestAuthHTTPStrictInput(t *testing.T) {
	_, h := authFixture(t)
	for _, c := range []struct {
		body   string
		status int
	}{
		{`{}`, 422}, {`null`, 400}, {`[]`, 400}, {`{"username":true,"password":"x"}`, 400},
		{`{"Username":"owner","password":"x"}`, 400},
		{`{"username":"owner","password":"x","role":"admin"}`, 400},
		{`{"username":"owner","username":"other","password":"x"}`, 400},
		{`{"username":"owner","password":"x"} {}`, 400},
		{`{"username":"owner","password":"` + strings.Repeat("x", 73) + `"}`, 422},
		{`{"username":"owner","password":"` + strings.Repeat("x", 20<<10) + `"}`, 413},
	} {
		w := requestAuth(h, "POST", "/api/v2/auth/login", c.body, testOrigin, "", nil)
		if w.Code != c.status {
			t.Errorf("body prefix %.80q status=%d want=%d: %s", c.body, w.Code, c.status, w.Body.String())
		}
	}
	r := httptest.NewRequest("POST", "/api/v2/auth/login", strings.NewReader(`{"username":"owner","password":"x"}`))
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal("simple cross-site content type accepted")
	}
}

func TestAuthHTTPPasswordRotationAndSessions(t *testing.T) {
	_, h := authFixture(t)
	cookie, csrf, user := loginHTTP(t, h)
	other, _, _ := loginHTTP(t, h)
	w := requestAuth(h, "GET", "/api/v2/auth/sessions?limit=1", "", "", "", cookie)
	var page sessionPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(page.Items) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("sessions: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), cookie.Value) || strings.Contains(w.Body.String(), csrf) {
		t.Fatal("session listing leaks capabilities")
	}
	w = requestAuth(h, "GET", "/api/v2/auth/sessions?limit=1&cursor="+*page.NextCursor, "", "", "", cookie)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var secondPage sessionPage
	if err := json.Unmarshal(w.Body.Bytes(), &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Items) != 1 || secondPage.HasMore || secondPage.Items[0].ID == page.Items[0].ID {
		t.Fatal("unstable session pagination")
	}
	wrongCursor := base64.RawURLEncoding.EncodeToString([]byte(`{"scope":"wrong","user":"` + user.ID + `","after":""}`))
	for _, query := range []string{"limit=0", "limit=101", "limit=no", "cursor=malformed", "cursor=" + wrongCursor} {
		w = requestAuth(h, "GET", "/api/v2/auth/sessions?"+query, "", "", "", cookie)
		if w.Code != 400 {
			t.Errorf("query %s accepted", query)
		}
	}
	input, _ := json.Marshal(passwordRequest{OldPassword: testPassword, NewPassword: "the new secure gallery password"})
	w = requestAuth(h, "POST", "/api/v2/auth/password", string(input), testOrigin, csrf, cookie)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("password: %d %s", w.Code, w.Body.String())
	}
	newCookie := w.Result().Cookies()[0]
	for _, old := range []*http.Cookie{cookie, other} {
		w = requestAuth(h, "GET", "/api/v2/auth/me", "", "", "", old)
		if w.Code != 401 {
			t.Fatal("old session survived password change")
		}
	}
	w = requestAuth(h, "GET", "/api/v2/auth/me", "", "", "", newCookie)
	if w.Code != 200 {
		t.Fatal("rotated current session rejected")
	}
	w = requestAuth(h, "POST", "/api/v2/auth/logout", "", testOrigin, csrf, newCookie)
	if w.Code != 403 {
		t.Fatal("old CSRF survived password change")
	}
}

func TestAuthHTTPDisabledExpiredRateLimitAndPrivacy(t *testing.T) {
	db, h := authFixture(t)
	cookie, _, user := loginHTTP(t, h)
	if _, err := db.Exec(`UPDATE users SET active=0 WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	w := requestAuth(h, "GET", "/api/v2/auth/me", "", "", "", cookie)
	if w.Code != 401 {
		t.Fatal("disabled user can read")
	}
	if _, err := db.Exec(`UPDATE users SET active=1 WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET expires_at=0`); err != nil {
		t.Fatal(err)
	}
	w = requestAuth(h, "GET", "/api/v2/auth/me", "", "", "", cookie)
	if w.Code != 401 {
		t.Fatal("expired session accepted")
	}
	if _, err := db.Exec(`UPDATE auth_rate_limits SET attempts=100,window_started_at=?`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(loginRequest{Username: "owner", Password: testPassword})
	w = requestAuth(h, "POST", "/api/v2/auth/login", string(body), testOrigin, "", nil)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d %s", w.Code, w.Body.String())
	}
	rows, err := db.Query(`SELECT detail_json FROM audit_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, testPassword) || strings.Contains(detail, cookie.Value) {
			t.Fatal("audit contains credentials")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestUnambiguousJSON(t *testing.T) {
	for _, data := range []string{`{"a":{"b":1,"b":2}}`, `{"a":1} trailing`, `{"a":[1,]}`, strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18)} {
		if unambiguousJSON([]byte(data)) == nil {
			t.Fatalf("ambiguous JSON accepted: %s", data)
		}
	}
	for _, data := range []string{`{"a":[{"b":1},{"b":2}],"c":true}`, `{"x":null}`, `{"num":123456789012345678901234567890}`} {
		if err := unambiguousJSON([]byte(data)); err != nil {
			t.Fatalf("valid JSON %s: %v", data, err)
		}
	}
}

func TestAuthHTTPRevokeEndpoint(t *testing.T) {
	_, h := authFixture(t)
	cookie, csrf, _ := loginHTTP(t, h)
	w := requestAuth(h, "GET", "/api/v2/auth/sessions", "", "", "", cookie)
	var page sessionPage
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(&page); err != nil {
		t.Fatal(err)
	}
	w = requestAuth(h, "DELETE", "/api/v2/auth/sessions/not-owned", "", testOrigin, csrf, cookie)
	if w.Code != 404 {
		t.Fatal("unknown session not hidden")
	}
	w = requestAuth(h, "DELETE", "/api/v2/auth/sessions/"+page.Items[0].ID, "", testOrigin, csrf, cookie)
	if w.Code != 204 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("current session revoke did not expire cookie")
	}
}
