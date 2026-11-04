package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

type adminClient struct {
	handler http.Handler
	cookie  *http.Cookie
	csrf    string
}

func (client adminClient) request(method, path string, input any, etag, key string) *httptest.ResponseRecorder {
	var body []byte
	if input != nil {
		body, _ = json.Marshal(input)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	r.RemoteAddr = "192.0.2.4:12345"
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("X-CSRF-Token", client.csrf)
	if len(body) != 0 {
		r.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if client.cookie != nil {
		r.AddCookie(client.cookie)
	}
	w := httptest.NewRecorder()
	client.handler.ServeHTTP(w, r)
	return w
}

func ownerClient(t *testing.T, h http.Handler) (adminClient, auth.User) {
	t.Helper()
	cookie, csrf, user := loginHTTP(t, h)
	return adminClient{h, cookie, csrf}, user
}

func accountClient(t *testing.T, h http.Handler, username, password string) adminClient {
	t.Helper()
	body, _ := json.Marshal(loginRequest{Username: username, Password: password})
	w := requestAuth(h, "POST", "/api/v2/auth/login", string(body), testOrigin, "", nil)
	if w.Code != 200 {
		t.Fatalf("account login: %d %s", w.Code, w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	w = requestAuth(h, "GET", "/api/v2/auth/csrf", "", "", "", cookie)
	var csrf csrfResponse
	if err := json.Unmarshal(w.Body.Bytes(), &csrf); err != nil || w.Code != 200 {
		t.Fatalf("csrf: %d %s %v", w.Code, w.Body.String(), err)
	}
	return adminClient{h, cookie, csrf.Token}
}

func responseUser(t *testing.T, w *httptest.ResponseRecorder, status int) auth.User {
	t.Helper()
	if w.Code != status {
		t.Fatalf("user response: %d want=%d %s", w.Code, status, w.Body.String())
	}
	var user auth.User
	if err := json.Unmarshal(w.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	if user.ID == "" || w.Header().Get("ETag") != userETag(user) || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("missing user identity/version/privacy: %+v %v", user, w.Header())
	}
	for _, secret := range []string{testPassword, "password_hash", "csrf_hash", "id_hash"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("credential leaked in user response")
		}
	}
	return user
}

func createHTTPUser(t *testing.T, client adminClient, username, role string) auth.User {
	t.Helper()
	return responseUser(t, client.request("POST", "/api/v2/admin/users", map[string]any{"username": username, "password": testPassword, "role": role}, "", store.NewID()), 201)
}

func expectCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	assertProblem(t, w, status)
	var body struct{ Code string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != code {
		t.Fatalf("problem code: %s want=%s %v", w.Body.String(), code, err)
	}
}

func TestAdminHTTPAccountLifecycle(t *testing.T) {
	_, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	input := map[string]any{"username": "Reader", "password": testPassword}
	key := store.NewID()
	created := admin.request("POST", "/api/v2/admin/users", input, "", key)
	user := responseUser(t, created, 201)
	path := "/api/v2/admin/users/" + user.ID
	if user.Role != "user" || !user.Active || user.DownloadOriginal || created.Header().Get("Location") != path {
		t.Fatal("incorrect account defaults or Location")
	}
	replayed := responseUser(t, admin.request("POST", "/api/v2/admin/users", input, "", key), 201)
	if replayed != user {
		t.Fatal("create was not idempotent")
	}
	input["password"] = testPassword + "different"
	expectCode(t, admin.request("POST", "/api/v2/admin/users", input, "", key), 409, "IDEMPOTENCY_CONFLICT")
	input["password"] = testPassword
	expectCode(t, admin.request("POST", "/api/v2/admin/users", input, "", store.NewID()), 409, "USERNAME_TAKEN")
	reader := accountClient(t, h, user.Username, testPassword)
	got := responseUser(t, admin.request("GET", path, nil, "", ""), 200)
	if got != user {
		t.Fatal("get did not return current metadata")
	}
	patch := map[string]any{"active": false, "download_original": true}
	expectCode(t, admin.request("PATCH", path, patch, "", ""), 428, "PRECONDITION_REQUIRED")
	disabled := responseUser(t, admin.request("PATCH", path, patch, userETag(user), ""), 200)
	if disabled.Active || !disabled.DownloadOriginal || disabled.ScopeID == user.ScopeID || disabled.Version != 2 {
		t.Fatal("policy change failed to rotate account scope")
	}
	expectCode(t, reader.request("GET", "/api/v2/auth/me", nil, "", ""), 401, "UNAUTHENTICATED")
	expectCode(t, admin.request("PATCH", path, map[string]bool{"active": true}, userETag(user), ""), 409, "VERSION_CONFLICT")
	active := responseUser(t, admin.request("PATCH", path, map[string]any{"active": true, "username": "renamed"}, userETag(disabled), ""), 200)
	expectCode(t, reader.request("GET", "/api/v2/auth/me", nil, "", ""), 401, "UNAUTHENTICATED")
	reader = accountClient(t, h, active.Username, testPassword)
	newPassword := "the administratively reset gallery passphrase"
	resetKey := store.NewID()
	resetInput := map[string]string{"new_password": newPassword}
	reset := responseUser(t, admin.request("POST", path+"/reset-password", resetInput, userETag(active), resetKey), 200)
	expectCode(t, reader.request("GET", "/api/v2/auth/me", nil, "", ""), 401, "UNAUTHENTICATED")
	reader = accountClient(t, h, active.Username, newPassword)
	responseUser(t, admin.request("POST", path+"/reset-password", resetInput, userETag(active), resetKey), 200)
	if w := reader.request("GET", "/api/v2/auth/me", nil, "", ""); w.Code != 200 {
		t.Fatal("reset replay revoked the new login")
	}
	expectCode(t, admin.request("DELETE", path, nil, userETag(active), ""), 409, "VERSION_CONFLICT")
	if w := admin.request("DELETE", path, nil, userETag(reset), ""); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	expectCode(t, reader.request("GET", "/api/v2/auth/me", nil, "", ""), 401, "UNAUTHENTICATED")
	expectCode(t, admin.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	ownerPath := "/api/v2/admin/users/" + owner.ID
	for _, patch := range []map[string]any{{"active": false}, {"role": "user"}} {
		expectCode(t, admin.request("PATCH", ownerPath, patch, userETag(owner), ""), 409, "LAST_ACTIVE_ADMIN")
	}
	expectCode(t, admin.request("DELETE", ownerPath, nil, userETag(owner), ""), 409, "LAST_ACTIVE_ADMIN")
}

func TestAdminHTTPAuthorizationAndCSRFMatrix(t *testing.T) {
	db, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	user := createHTTPUser(t, admin, "reader", "user")
	reader := accountClient(t, h, user.Username, testPassword)
	for _, op := range Operations {
		if !strings.HasPrefix(op.Path, "/admin/") {
			continue
		}
		path := strings.ReplaceAll(strings.ReplaceAll("/api/v2"+op.Path, "{user_id}", owner.ID), "{session_id}", "not-owned")
		expectCode(t, reader.request(op.Method, path, map[string]bool{}, userETag(owner), store.NewID()), 403, "ADMIN_REQUIRED")
		if op.Method != "GET" {
			w := requestAuth(h, op.Method, path, `{}`, testOrigin, "", admin.cookie)
			expectCode(t, w, 403, "CSRF_REJECTED")
			w = requestAuth(h, op.Method, path, `{}`, "https://untrusted.test", admin.csrf, admin.cookie)
			expectCode(t, w, 403, "ORIGIN_REJECTED")
		}
	}
	if _, err := db.Exec(`UPDATE users SET role='user' WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", "/api/v2/admin/users", nil, "", ""), 403, "ADMIN_REQUIRED")
	if _, err := db.Exec(`UPDATE users SET role='admin',active=0 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", "/api/v2/admin/audit", nil, "", ""), 401, "UNAUTHENTICATED")
	if _, err := db.Exec(`UPDATE users SET active=1 WHERE id=?; UPDATE sessions SET expires_at=0`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", "/api/v2/admin/users", nil, "", ""), 401, "UNAUTHENTICATED")
}

func TestAdminHTTPStrictFieldsPreconditionsAndMethodErrors(t *testing.T) {
	_, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	path := "/api/v2/admin/users/" + owner.ID
	for _, input := range []any{
		map[string]any{"active": nil, "role": "admin"}, map[string]any{"active": "false"},
		map[string]any{"Active": true}, map[string]any{"scope_id": "forged"}, map[string]any{"version": 999},
		map[string]any{"password": testPassword},
	} {
		expectCode(t, admin.request("PATCH", path, input, userETag(owner), ""), 400, "INVALID_JSON")
	}
	for _, input := range []any{map[string]any{}, map[string]any{"role": "superuser"}, map[string]any{"username": strings.Repeat("a", 65)}} {
		expectCode(t, admin.request("PATCH", path, input, userETag(owner), ""), 422, "VALIDATION_FAILED")
	}
	for _, value := range []string{"*", `W/` + userETag(owner), `"1"`, `"user:wrong:1"`, `"user:` + owner.ID + `:+1"`, userETag(owner) + ", " + userETag(owner), `"user:` + owner.ID + `:9223372036854775808"`} {
		expectCode(t, admin.request("PATCH", path, map[string]bool{"active": true}, value, ""), 400, "INVALID_PRECONDITION")
	}
	expectCode(t, admin.request("POST", "/api/v2/admin/users", map[string]string{"username": "reader", "password": testPassword}, "", ""), 422, "VALIDATION_FAILED")
	for _, role := range []string{"", "owner"} {
		expectCode(t, admin.request("POST", "/api/v2/admin/users", map[string]string{"username": "reader", "password": testPassword, "role": role}, "", store.NewID()), 422, "VALIDATION_FAILED")
	}
	for _, method := range []string{"PUT", "OPTIONS", "POST"} {
		w := admin.request(method, path, nil, "", "")
		expectCode(t, w, 405, "METHOD_NOT_ALLOWED")
		if w.Header().Get("Allow") != "GET, HEAD, PATCH, DELETE" {
			t.Fatalf("incorrect parameterized Allow: %s", w.Header().Get("Allow"))
		}
	}
	r := httptest.NewRequest("PATCH", path, strings.NewReader(`{"active":true}`))
	r.AddCookie(admin.cookie)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("X-CSRF-Token", admin.csrf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Add("If-Match", userETag(owner))
	r.Header.Add("If-Match", userETag(owner))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	expectCode(t, w, 400, "INVALID_PRECONDITION")
}

func TestAdminHTTPKeysetPaginationSessionsAndAudit(t *testing.T) {
	_, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	user := createHTTPUser(t, admin, "reader", "user")
	reader := accountClient(t, h, user.Username, testPassword)
	accountClient(t, h, user.Username, testPassword)
	otherAdmin := createHTTPUser(t, admin, "another-admin", "admin")
	other := accountClient(t, h, otherAdmin.Username, testPassword)
	w := admin.request("GET", "/api/v2/admin/users?limit=1", nil, "", "")
	var page listPage[auth.User]
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || w.Code != 200 || len(page.Items) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatalf("users page: %s %v", w.Body.String(), err)
	}
	if page.Items[0].ID != owner.ID {
		t.Fatal("unstable user ordering")
	}
	cursor := *page.NextCursor
	w = admin.request("GET", "/api/v2/admin/users?limit=1&cursor="+cursor, nil, "", "")
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != user.ID {
		t.Fatalf("next users page: %s %v", w.Body.String(), err)
	}
	expectCode(t, other.request("GET", "/api/v2/admin/users?cursor="+cursor, nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, admin.request("GET", "/api/v2/admin/audit?cursor="+cursor, nil, "", ""), 400, "INVALID_CURSOR")
	for _, query := range []string{"limit=101", "limit=0", "limit=no", "limit=1&limit=2", "limit=", "limit=%FF", "cursor=x&cursor=y"} {
		expectCode(t, admin.request("GET", "/api/v2/admin/users?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	sessionPath := "/api/v2/admin/users/" + user.ID + "/sessions"
	w = admin.request("GET", sessionPath+"?limit=1", nil, "", "")
	var sessions sessionPage
	if err := json.Unmarshal(w.Body.Bytes(), &sessions); err != nil || w.Code != 200 || len(sessions.Items) != 1 || !sessions.HasMore || sessions.Items[0].Current {
		t.Fatalf("admin sessions: %s %v", w.Body.String(), err)
	}
	id := sessions.Items[0].ID
	if strings.Contains(w.Body.String(), reader.cookie.Value) || strings.Contains(w.Body.String(), reader.csrf) {
		t.Fatal("admin session list leaked capabilities")
	}
	expectCode(t, admin.request("DELETE", "/api/v2/admin/users/"+owner.ID+"/sessions/"+id, nil, "", ""), 404, "NOT_FOUND")
	if w := admin.request("DELETE", sessionPath+"/"+id, nil, "", ""); w.Code != 204 {
		t.Fatalf("admin revoke: %d %s", w.Code, w.Body.String())
	}
	expectCode(t, reader.request("GET", "/api/v2/auth/me", nil, "", ""), 401, "UNAUTHENTICATED")
	w = admin.request("GET", "/api/v2/admin/audit?limit=1", nil, "", "")
	var audit listPage[auth.AuditEvent]
	if err := json.Unmarshal(w.Body.Bytes(), &audit); err != nil || w.Code != 200 || len(audit.Items) != 1 || audit.Items[0].EventType != "admin_session_revoked" || !audit.HasMore {
		t.Fatalf("audit page: %s %v", w.Body.String(), err)
	}
	firstID, _ := strconv.ParseInt(audit.Items[0].ID, 10, 64)
	w = admin.request("GET", "/api/v2/admin/audit?limit=1&cursor="+*audit.NextCursor, nil, "", "")
	if err := json.Unmarshal(w.Body.Bytes(), &audit); err != nil || len(audit.Items) != 1 {
		t.Fatalf("next audit page: %s %v", w.Body.String(), err)
	}
	nextID, _ := strconv.ParseInt(audit.Items[0].ID, 10, 64)
	if nextID >= firstID {
		t.Fatal("audit is not newest-first keyset pagination")
	}
	changed := responseUser(t, admin.request("PATCH", "/api/v2/admin/users/"+owner.ID, map[string]bool{"download_original": false}, userETag(owner), ""), 200)
	admin = accountClient(t, h, owner.Username, testPassword)
	if changed.ScopeID == owner.ScopeID {
		t.Fatal("policy update did not rotate the scope")
	}
	expectCode(t, admin.request("GET", "/api/v2/admin/users?cursor="+cursor, nil, "", ""), 400, "INVALID_CURSOR")
}

func TestAdminHTTPSelfResetReplayKeepsNewSession(t *testing.T) {
	_, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	path := "/api/v2/admin/users/" + owner.ID + "/reset-password"
	key := store.NewID()
	input := map[string]string{"new_password": "a new owner account security passphrase"}
	w := admin.request("POST", path, input, userETag(owner), key)
	responseUser(t, w, 200)
	if len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("self-reset did not expire the revoked cookie")
	}
	fresh := accountClient(t, h, owner.Username, input["new_password"])
	w = fresh.request("POST", path, input, userETag(owner), key)
	responseUser(t, w, 200)
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("idempotent replay expired a valid new session")
	}
	if w := fresh.request("GET", "/api/v2/auth/me", nil, "", ""); w.Code != 200 {
		t.Fatal("self-reset replay revoked the new session")
	}
}
