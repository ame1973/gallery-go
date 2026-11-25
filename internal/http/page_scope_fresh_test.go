package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func TestListHandlersRejectScopeChangeAfterAuthentication(t *testing.T) {
	db, router := authFixture(t)
	admin, _ := ownerClient(t, router)
	createHTTPUser(t, admin, "reader-one", "user")
	createHTTPUser(t, admin, "reader-two", "user")
	loginHTTP(t, router)

	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	accessService := access.New(db, service)
	queue, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	initial := admin.request(http.MethodGet, "/api/v2/admin/users?limit=1", nil, "", "")
	var first listPage[auth.User]
	if err := json.Unmarshal(initial.Body.Bytes(), &first); err != nil || initial.Code != http.StatusOK || first.NextCursor == nil {
		t.Fatalf("initial users page status=%d cursor=%v error=%v", initial.Code, first.NextCursor != nil, err)
	}

	tests := []struct {
		name, path, method string
		serve              http.HandlerFunc
	}{
		{"users-with-cursor", "/api/v2/admin/users?limit=1&cursor=" + url.QueryEscape(*first.NextCursor), http.MethodGet, (authentication{service: service}).users},
		{"audit", "/api/v2/admin/audit?limit=1", http.MethodGet, (authentication{service: service}).audit},
		{"admin-groups", "/api/v2/admin/groups?limit=1", http.MethodGet, (authorization{service: accessService}).groups},
		{"visible-libraries", "/api/v2/libraries?limit=1", http.MethodGet, (authorization{service: accessService}).libraries},
		{"jobs", "/api/v2/admin/jobs?limit=1", http.MethodGet, (jobHandler{service: jobs.NewAdmin(queue, service)}).list},
		{"head-users", "/api/v2/admin/users?limit=1", http.MethodHead, (authentication{service: service}).users},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			protected := (authentication{service: service}).protect(func(w http.ResponseWriter, r *http.Request) {
				captured := currentSession(r)
				if _, err := db.ExecContext(r.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, store.NewID(), captured.User.ID); err != nil {
					t.Fatal(err)
				}
				test.serve(w, r)
			})
			request := httptest.NewRequest(test.method, test.path, nil)
			request.AddCookie(admin.cookie)
			response := httptest.NewRecorder()
			response.Header().Set("X-Request-ID", "scope-interleave-test")
			protected(response, request)
			if response.Code != http.StatusConflict {
				var page listPage[json.RawMessage]
				_ = json.Unmarshal(response.Body.Bytes(), &page)
				t.Errorf("scope changed after middleware: status=%d items=%d next_cursor=%v; want 409", response.Code, len(page.Items), page.NextCursor != nil)
				return
			}
			assertProblem(t, response, http.StatusConflict)
		})
	}
	stale := admin.request(http.MethodGet, "/api/v2/admin/users?cursor="+url.QueryEscape(*first.NextCursor), nil, "", "")
	if stale.Code != http.StatusBadRequest {
		t.Fatalf("old cursor after refresh returned %d", stale.Code)
	}
	fresh := admin.request(http.MethodGet, "/api/v2/admin/users?limit=1", nil, "", "")
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh page after scope change returned %d", fresh.Code)
	}
}

func TestListHandlerRejectsRealMembershipChangeAfterAuthentication(t *testing.T) {
	db, router := authFixture(t)
	admin, owner := ownerClient(t, router)
	createHTTPUser(t, admin, "reader", "user")
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.Authenticate(t.Context(), admin.cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	accessService := access.New(db, service)
	group, err := accessService.CreateGroup(t.Context(), actor, "scope test", store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	membership, err := accessService.UserGroups(t.Context(), actor, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	protected := (authentication{service: service}).protect(func(w http.ResponseWriter, r *http.Request) {
		updated, err := accessService.SetUserGroups(t.Context(), currentSession(r), owner.ID, membership.Version, []string{group.ID})
		if err != nil || updated.ScopeID == membership.ScopeID {
			t.Fatalf("real membership change: scope changed=%v error=%v", updated.ScopeID != membership.ScopeID, err)
		}
		(authentication{service: service}).users(w, r)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v2/admin/users?limit=1", nil)
	request.AddCookie(admin.cookie)
	response := httptest.NewRecorder()
	response.Header().Set("X-Request-ID", "membership-scope-test")
	protected(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("real membership change returned status %d", response.Code)
	}
	assertProblem(t, response, http.StatusConflict)
	fresh := admin.request(http.MethodGet, "/api/v2/admin/users?limit=1", nil, "", "")
	if fresh.Code != http.StatusOK {
		t.Fatalf("fresh page after real membership change returned %d", fresh.Code)
	}
}

func TestMediaReadRejectsScopeChangeAfterAuthentication(t *testing.T) {
	f := protectedMedia(t)
	service, err := auth.New(f.db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, method, rangeHeader string
	}{
		{"get", http.MethodGet, ""},
		{"head", http.MethodHead, ""},
		{"range", http.MethodGet, "bytes=0-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			protected := (authentication{service: service}).protect(func(w http.ResponseWriter, r *http.Request) {
				captured := currentSession(r)
				if _, err := f.db.ExecContext(r.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, store.NewID(), captured.User.ID); err != nil {
					t.Fatal(err)
				}
				(mediaHandler{service: f.service}).variantContent(w, r)
			})
			request := httptest.NewRequest(test.method, f.variantURL, nil)
			request.AddCookie(f.reader.cookie)
			request.SetPathValue("media_id", "protected")
			request.SetPathValue("variant_id", "protected-variant")
			if test.rangeHeader != "" {
				request.Header.Set("Range", test.rangeHeader)
			}
			response := httptest.NewRecorder()
			response.Header().Set("X-Request-ID", "media-scope-test")
			protected(response, request)
			if response.Code != http.StatusConflict {
				t.Fatalf("stale media %s returned status %d", test.name, response.Code)
			}
			assertProblem(t, response, http.StatusConflict)
			if response.Header().Get("Content-Range") != "" {
				t.Fatal("stale read returned a content range")
			}
		})
	}
	fresh := mediaRequest(f.reader, http.MethodGet, f.variantURL, http.Header{"Range": {"bytes=0-1"}})
	if fresh.Code != http.StatusPartialContent || fresh.Body.Len() != 2 {
		t.Fatalf("fresh range after scope change: status=%d bytes=%d", fresh.Code, fresh.Body.Len())
	}
}

func TestUnsafeRequestsRetainFreshAuthoritySemantics(t *testing.T) {
	db, router := authFixture(t)
	cookie, csrf, _ := loginHTTP(t, router)
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	protected := (authentication{service: service, origin: testOrigin}).protect(func(w http.ResponseWriter, r *http.Request) {
		current := currentSession(r)
		if _, err := db.ExecContext(r.Context(), `UPDATE users SET scope_id=?,version=version+1 WHERE id=?`, "unsafe-new-scope", current.User.ID); err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		fresh, err := service.SessionInTx(r.Context(), tx, current)
		if err != nil || fresh.User.ScopeID != "unsafe-new-scope" {
			t.Fatalf("unsafe request did not use fresh authority: scope=%q error=%v", fresh.User.ScopeID, err)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/fixture", nil)
	request.AddCookie(cookie)
	request.Header.Set("Origin", testOrigin)
	request.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	protected(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("unsafe request returned status %d", response.Code)
	}
}
