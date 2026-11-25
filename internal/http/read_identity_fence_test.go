package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

const (
	expectedReadUser  = "X-GalleryGo-Expected-User"
	expectedReadScope = "X-GalleryGo-Expected-Scope"
)

func requestWithReadIdentity(h http.Handler, method, path string, cookie *http.Cookie, csrf string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "192.0.2.11:12345"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("X-CSRF-Token", csrf)
	}
	for name, values := range headers {
		for _, value := range values {
			r.Header.Add(name, value)
		}
	}
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "read-identity-test")
	h.ServeHTTP(w, r)
	return w
}

func TestReadIdentityFenceRejectsWrongAccountAndScope(t *testing.T) {
	_, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	reader := createHTTPUser(t, admin, "identity-reader", "user")
	readerClient := accountClient(t, h, reader.Username, testPassword)
	for _, test := range []struct {
		name, user, scope string
		cookie            *http.Cookie
	}{
		{"other-account", owner.ID, owner.ScopeID, readerClient.cookie},
		{"other-scope", owner.ID, "different-scope", admin.cookie},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := requestWithReadIdentity(h, http.MethodGet, "/api/v2/auth/me", test.cookie, "", http.Header{expectedReadUser: {test.user}, expectedReadScope: {test.scope}})
			if response.Code != http.StatusConflict {
				t.Errorf("identity mismatch returned status %d; want 409", response.Code)
				return
			}
			expectCode(t, response, http.StatusConflict, "VERSION_CONFLICT")
			if response.Header().Get(expectedReadUser) != "" || response.Header().Get(expectedReadScope) != "" {
				t.Fatal("identity mismatch echoed an account")
			}
			if strings.Contains(response.Body.String(), reader.ID) || strings.Contains(response.Body.String(), reader.Username) {
				t.Fatal("identity mismatch disclosed the actual account")
			}
		})
	}
}

func TestReadIdentityFenceRequiresCompleteSingleBoundedHeaders(t *testing.T) {
	_, h := authFixture(t)
	cookie, _, user := loginHTTP(t, h)
	for _, test := range []struct {
		name    string
		headers http.Header
	}{
		{"missing-user", http.Header{expectedReadScope: {user.ScopeID}}},
		{"missing-scope", http.Header{expectedReadUser: {user.ID}}},
		{"empty-user", http.Header{expectedReadUser: {""}, expectedReadScope: {user.ScopeID}}},
		{"empty-scope", http.Header{expectedReadUser: {user.ID}, expectedReadScope: {""}}},
		{"duplicate-user", http.Header{expectedReadUser: {user.ID, user.ID}, expectedReadScope: {user.ScopeID}}},
		{"duplicate-scope", http.Header{expectedReadUser: {user.ID}, expectedReadScope: {user.ScopeID, user.ScopeID}}},
		{"long-user", http.Header{expectedReadUser: {strings.Repeat("a", 129)}, expectedReadScope: {user.ScopeID}}},
		{"long-scope", http.Header{expectedReadUser: {user.ID}, expectedReadScope: {strings.Repeat("a", 129)}}},
		{"space-user", http.Header{expectedReadUser: {"not a valid id"}, expectedReadScope: {user.ScopeID}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := requestWithReadIdentity(h, http.MethodGet, "/api/v2/auth/me", cookie, "", test.headers)
			if response.Code != http.StatusBadRequest {
				t.Errorf("malformed identity headers returned status %d; want 400", response.Code)
				return
			}
			expectCode(t, response, http.StatusBadRequest, "INVALID_READ_CONTEXT")
			if response.Header().Get(expectedReadUser) != "" || response.Header().Get(expectedReadScope) != "" {
				t.Fatal("invalid identity headers were echoed")
			}
		})
	}
}

func TestReadIdentityFenceEchoAndUnboundCompatibility(t *testing.T) {
	_, h := authFixture(t)
	cookie, _, user := loginHTTP(t, h)
	expected := http.Header{expectedReadUser: {user.ID}, expectedReadScope: {user.ScopeID}}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		response := requestWithReadIdentity(h, method, "/api/v2/admin/users?limit=1", cookie, "", expected)
		if response.Code != http.StatusOK {
			t.Fatalf("bound %s returned %d", method, response.Code)
		}
		if response.Header().Get(expectedReadUser) != user.ID || response.Header().Get(expectedReadScope) != user.ScopeID {
			t.Errorf("bound %s did not echo the validated identity", method)
		}
		if response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("bound response was cacheable")
		}
	}
	unbound := requestWithReadIdentity(h, http.MethodGet, "/api/v2/admin/users?limit=1", cookie, "", nil)
	if unbound.Code != http.StatusOK || unbound.Header().Get(expectedReadUser) != "" || unbound.Header().Get(expectedReadScope) != "" {
		t.Fatalf("unbound API compatibility: status=%d", unbound.Code)
	}
	anonymous := requestWithReadIdentity(h, http.MethodGet, "/api/v2/admin/users?limit=1", nil, "", expected)
	if anonymous.Code != http.StatusUnauthorized || anonymous.Header().Get(expectedReadUser) != "" || anonymous.Header().Get(expectedReadScope) != "" {
		t.Fatalf("identity headers authorized an anonymous caller: status=%d", anonymous.Code)
	}
}

func TestReadIdentityFenceStillRejectsTransactionalScopeChange(t *testing.T) {
	db, h := authFixture(t)
	cookie, _, user := loginHTTP(t, h)
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	protected := (authentication{service: service}).protect(func(w http.ResponseWriter, r *http.Request) {
		captured := currentSession(r)
		if _, err := db.ExecContext(r.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, store.NewID(), captured.User.ID); err != nil {
			t.Fatal(err)
		}
		(authentication{service: service}).users(w, r)
	})
	response := requestWithReadIdentity(protected, http.MethodGet, "/api/v2/admin/users?limit=1", cookie, "", http.Header{expectedReadUser: {user.ID}, expectedReadScope: {user.ScopeID}})
	if response.Code != http.StatusConflict {
		t.Fatalf("later scope change returned status %d", response.Code)
	}
	expectCode(t, response, http.StatusConflict, "VERSION_CONFLICT")
}

func TestReadIdentityValueBounds(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"a", true}, {strings.Repeat("a", 128), true},
		{"", false}, {strings.Repeat("a", 129), false},
		{"white space", false}, {"tab\tchar", false}, {"line\nfeed", false},
		{"control\x00", false}, {"delete\x7f", false}, {"東京", false},
	} {
		if got := readIdentityValue(test.value); got != test.valid {
			t.Errorf("identity value %q: got=%v want=%v", test.value, got, test.valid)
		}
	}
}

func TestReadIdentityFenceCannotBeUsedOnMutation(t *testing.T) {
	_, h := authFixture(t)
	cookie, csrf, user := loginHTTP(t, h)
	for name, headers := range map[string]http.Header{
		"both":       {expectedReadUser: {user.ID}, expectedReadScope: {user.ScopeID}},
		"only-user":  {expectedReadUser: {user.ID}},
		"only-scope": {expectedReadScope: {user.ScopeID}},
	} {
		t.Run(name, func(t *testing.T) {
			response := requestWithReadIdentity(h, http.MethodPost, "/api/v2/auth/logout", cookie, csrf, headers)
			if response.Code != http.StatusBadRequest {
				t.Errorf("read-only expectations on logout returned status %d; want 400", response.Code)
			} else {
				expectCode(t, response, http.StatusBadRequest, "INVALID_READ_CONTEXT")
			}
			if len(response.Result().Cookies()) != 0 {
				t.Error("rejected logout changed a cookie")
			}
			if response.Header().Get(expectedReadUser) != "" || response.Header().Get(expectedReadScope) != "" {
				t.Error("rejected logout echoed a read context")
			}
			current := requestAuth(h, http.MethodGet, "/api/v2/auth/me", "", "", "", cookie)
			if current.Code != http.StatusOK {
				t.Errorf("rejected logout changed the session: status=%d", current.Code)
			}
		})
	}
}

func TestReadIdentityFencePreservesMediaRanges(t *testing.T) {
	f := protectedMedia(t)
	expected := http.Header{expectedReadUser: {f.user.ID}, expectedReadScope: {f.user.ScopeID}, "Range": {"bytes=0-1"}}
	response := requestWithReadIdentity(f.http, http.MethodGet, f.variantURL, f.reader.cookie, "", expected)
	if response.Code != http.StatusPartialContent || response.Body.Len() != 2 {
		t.Fatalf("bound range status=%d bytes=%d", response.Code, response.Body.Len())
	}
	if response.Header().Get(expectedReadUser) != f.user.ID || response.Header().Get(expectedReadScope) != f.user.ScopeID {
		t.Error("bound range did not echo validated identity")
	}
	assertPrivateContent(t, response)
	notModified := requestWithReadIdentity(f.http, http.MethodGet, f.variantURL, f.reader.cookie, "", http.Header{expectedReadUser: {f.user.ID}, expectedReadScope: {f.user.ScopeID}, "If-None-Match": {response.Header().Get("ETag")}})
	if notModified.Code != http.StatusNotModified || notModified.Header().Get(expectedReadUser) != f.user.ID || notModified.Header().Get(expectedReadScope) != f.user.ScopeID {
		t.Fatalf("bound conditional read status=%d", notModified.Code)
	}
	assertPrivateContent(t, notModified)
}
