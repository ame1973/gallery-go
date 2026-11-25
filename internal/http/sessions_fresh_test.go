package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gallerygo/internal/auth"
)

func TestSessionsHandlerRejectsRevocationAfterAuthentication(t *testing.T) {
	db, handler := authFixture(t)
	cookie, _, _ := loginHTTP(t, handler)
	loginHTTP(t, handler)
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	captured, err := service.Authenticate(t.Context(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(t.Context(), captured, captured.ID); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v2/auth/sessions", nil)
	request = request.WithContext(context.WithValue(request.Context(), sessionContextKey{}, captured))
	response := httptest.NewRecorder()
	response.Header().Set("X-Request-ID", "held-session-test")
	(authentication{service: service}).sessions(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked caller received status %d", response.Code)
	}
	assertProblem(t, response, http.StatusUnauthorized)
}

func TestSessionsHTTPRejectsCursorFromPreviousScope(t *testing.T) {
	db, handler := authFixture(t)
	cookie, _, user := loginHTTP(t, handler)
	loginHTTP(t, handler)
	first := requestAuth(handler, http.MethodGet, "/api/v2/auth/sessions?limit=1", "", "", "", cookie)
	var page sessionPage
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil || first.Code != http.StatusOK || page.NextCursor == nil {
		t.Fatalf("initial session page: status=%d error=%v", first.Code, err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, "fresh-test-scope", user.ID); err != nil {
		t.Fatal(err)
	}
	stale := requestAuth(handler, http.MethodGet, "/api/v2/auth/sessions?limit=1&cursor="+*page.NextCursor, "", "", "", cookie)
	if stale.Code != http.StatusBadRequest {
		t.Fatalf("stale scope cursor received status %d", stale.Code)
	}
	assertProblem(t, stale, http.StatusBadRequest)
	fresh := requestAuth(handler, http.MethodGet, "/api/v2/auth/sessions?limit=1", "", "", "", cookie)
	if fresh.Code != http.StatusOK {
		t.Fatalf("refreshed session page received status %d", fresh.Code)
	}
}

func TestSessionsHandlerRejectsScopeChangeAfterAuthentication(t *testing.T) {
	db, handler := authFixture(t)
	cookie, _, _ := loginHTTP(t, handler)
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	captured, err := service.Authenticate(t.Context(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, "fresh-test-scope", captured.User.ID); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v2/auth/sessions", nil)
	request = request.WithContext(context.WithValue(request.Context(), sessionContextKey{}, captured))
	response := httptest.NewRecorder()
	response.Header().Set("X-Request-ID", "held-session-test")
	(authentication{service: service}).sessions(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale scope received status %d", response.Code)
	}
	assertProblem(t, response, http.StatusConflict)
}
