package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestTagCatalogHTTPCommandsRequireVersionsAndReplayHistoricalResults(t *testing.T) {
	f := protectedMedia(t)
	const collection = "/api/v2/admin/tags"
	key := store.NewID()
	createdResponse := f.admin.request("POST", collection, map[string]string{"name": "  旅行  "}, "", key)
	created := decodeResponse[access.Tag](t, createdResponse, 201)
	path := collection + "/" + created.ID
	if created.Name != "旅行" || created.Version != 1 || createdResponse.Header().Get("ETag") != resourceETag("tag", created.ID, 1) || createdResponse.Header().Get("Location") != path || createdResponse.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("tag creation did not return exact versioned private metadata", created, createdResponse.Header())
	}
	expectCode(t, f.admin.request("POST", collection, map[string]string{"name": "旅行"}, "", store.NewID()), 409, "TAG_NAME_TAKEN")
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	me := decodeResponse[auth.User](t, f.admin.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	renameKey := store.NewID()
	changedResponse := f.admin.request("PATCH", path, map[string]string{"name": "Renamed"}, createdResponse.Header().Get("ETag"), renameKey)
	changed := decodeResponse[access.Tag](t, changedResponse, 200)
	if changed.ID != created.ID || changed.Name != "Renamed" || changed.Version != 2 || changedResponse.Header().Get("ETag") != resourceETag("tag", created.ID, 2) {
		t.Fatal("tag rename lost identity/version or depended on source reachability", changed, changedResponse.Header())
	}
	noop := decodeResponse[access.Tag](t, f.admin.request("PATCH", path, map[string]string{"name": " Renamed "}, changedResponse.Header().Get("ETag"), store.NewID()), 200)
	if noop != changed {
		t.Fatal("identical tag label changed the business version", noop, changed)
	}
	if current := decodeResponse[access.Tag](t, f.admin.request("GET", path, nil, "", ""), 200); current != changed {
		t.Fatal("current tag GET differs from its committed version", current, changed)
	}
	replay := f.admin.request("POST", collection, map[string]string{"name": "旅行"}, "", key)
	if value := decodeResponse[access.Tag](t, replay, 201); value != created || replay.Header().Get("ETag") != createdResponse.Header().Get("ETag") {
		t.Fatal("creation replay was retargeted by a later rename", value, replay.Header())
	}
	expectCode(t, f.admin.request("PATCH", path, map[string]string{"name": "New stale"}, createdResponse.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	expectCode(t, f.admin.request("DELETE", path, nil, createdResponse.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	expectCode(t, f.admin.request("PATCH", path, map[string]string{"name": "Different"}, createdResponse.Header().Get("ETag"), renameKey), 409, "IDEMPOTENCY_CONFLICT")
	deleteKey := store.NewID()
	for range 2 {
		deleted := f.admin.request("DELETE", path, nil, changedResponse.Header().Get("ETag"), deleteKey)
		if deleted.Code != 204 || deleted.Body.Len() != 0 || deleted.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("body-free deletion was not safely replayable", deleted.Code, deleted.Body.String(), deleted.Header())
		}
	}
	expectCode(t, f.admin.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.admin.request("DELETE", path, nil, changedResponse.Header().Get("ETag"), store.NewID()), 404, "NOT_FOUND")
	if value := decodeResponse[access.Tag](t, f.admin.request("PATCH", path, map[string]string{"name": "Renamed"}, createdResponse.Header().Get("ETag"), renameKey), 200); value != changed {
		t.Fatal("rename receipt lost historical metadata after deletion", value, changed)
	}
	currentMe := decodeResponse[auth.User](t, f.admin.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	if currentMe.ScopeID != me.ScopeID {
		t.Fatal("catalog metadata commands changed authorization scope", me.ScopeID, currentMe.ScopeID)
	}
	for _, secret := range []string{f.source, "root_path", "relative_path", "source_config", "marker"} {
		if strings.Contains(createdResponse.Body.String()+changedResponse.Body.String(), secret) {
			t.Fatal("tag catalog metadata exposed private filesystem context", secret)
		}
	}
}

func TestTagCatalogHTTPPagesAreBoundedPrivateAndScopeCollectionBound(t *testing.T) {
	f := protectedMedia(t)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 105 {
		if _, err := tx.Exec(`INSERT INTO tags(id,name) VALUES(?,?)`, fmt.Sprintf("tag-%03d", index), fmt.Sprintf("Unused private label %03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v2/admin/tags"
	page := decodeResponse[listPage[access.Tag]](t, f.admin.request("GET", path, nil, "", ""), 200)
	if len(page.Items) != 30 || !page.HasMore || page.NextCursor == nil {
		t.Fatal("tag page did not use bounded default keysets", page)
	}
	first := decodeResponse[listPage[access.Tag]](t, f.admin.request("GET", path+"?limit=100", nil, "", ""), 200)
	last := decodeResponse[listPage[access.Tag]](t, f.admin.request("GET", path+"?limit=100&cursor="+*first.NextCursor, nil, "", ""), 200)
	if len(first.Items) != 100 || len(last.Items) != 5 || last.HasMore || last.NextCursor != nil || last.Items[4].ID != "tag-104" {
		t.Fatal("tag keysets omitted or duplicated catalog rows", first, last)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "cursor=x&cursor=y"} {
		expectCode(t, f.admin.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	expectCode(t, f.admin.request("GET", path+"?name=private", nil, "", ""), 400, "INVALID_FILTER")
	expectCode(t, f.admin.request("GET", path+"?cursor=bad", nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, f.admin.request("GET", "/api/v2/admin/groups?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	me := decodeResponse[auth.User](t, f.admin.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	if _, err := f.db.Exec(`UPDATE users SET scope_id='new-tag-catalog-scope',version=version+1 WHERE id=?`, me.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("GET", path+"?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
}

func TestTagCatalogHTTPRejectsUnsafeBodiesRolesAssociationsAndLateFailures(t *testing.T) {
	f := protectedMedia(t)
	const collection = "/api/v2/admin/tags"
	created := decodeResponse[access.Tag](t, f.admin.request("POST", collection, map[string]string{"name": "Assigned"}, "", store.NewID()), 201)
	path := collection + "/" + created.ID
	tag := resourceETag("tag", created.ID, 1)
	for _, client := range []struct {
		value  adminClient
		status int
		code   string
	}{{adminClient{handler: f.http}, 401, "UNAUTHENTICATED"}, {f.reader, 403, "ADMIN_REQUIRED"}, {adminClient{handler: f.http, cookie: &http.Cookie{Name: sessionCookie, Value: strings.Repeat("x", 43)}}, 401, "UNAUTHENTICATED"}} {
		for _, route := range []string{collection, path, collection + "/missing"} {
			expectCode(t, client.value.request("GET", route, nil, "", ""), client.status, client.code)
		}
		expectCode(t, client.value.request("POST", collection, map[string]string{"name": "Denied"}, "", store.NewID()), client.status, client.code)
		expectCode(t, client.value.request("PATCH", path, map[string]string{"name": "Denied"}, tag, store.NewID()), client.status, client.code)
		expectCode(t, client.value.request("DELETE", path, nil, tag, store.NewID()), client.status, client.code)
	}
	for _, body := range []map[string]any{{"name": nil}, {"name": 1}, {"Name": "ambiguous"}, {"id": "other"}, {"version": 7}, {"album_ids": []string{"media-album"}}} {
		expectCode(t, f.admin.request("POST", collection, body, "", store.NewID()), 400, "INVALID_JSON")
		expectCode(t, f.admin.request("PATCH", path, body, tag, store.NewID()), 400, "INVALID_JSON")
	}
	for _, body := range []map[string]any{{}, {"name": "  "}, {"name": strings.Repeat("字", 101)}, {"name": "bad\x00label"}} {
		expectCode(t, f.admin.request("POST", collection, body, "", store.NewID()), 422, "VALIDATION_FAILED")
		expectCode(t, f.admin.request("PATCH", path, body, tag, store.NewID()), 422, "VALIDATION_FAILED")
	}
	for _, method := range []string{"PATCH", "DELETE"} {
		body := any(nil)
		if method == "PATCH" {
			body = map[string]string{"name": "Name"}
		}
		expectCode(t, f.admin.request(method, path, body, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
		expectCode(t, f.admin.request(method, path, body, tag, ""), 422, "VALIDATION_FAILED")
		for _, invalid := range []string{"*", "W/" + tag, resourceETag("tag", "other", 1), resourceETag("person", created.ID, 1)} {
			expectCode(t, f.admin.request(method, path, body, invalid, store.NewID()), 400, "INVALID_PRECONDITION")
		}
	}
	expectCode(t, f.admin.request("POST", collection, map[string]string{"name": "No key"}, "", ""), 422, "VALIDATION_FAILED")
	expectCode(t, f.admin.request("DELETE", path, map[string]any{}, tag, store.NewID()), 400, "INVALID_BODY")
	expectCode(t, requestAuth(f.http, "POST", collection, `{"name":"Denied"}`, testOrigin, "", f.admin.cookie), 403, "CSRF_REJECTED")
	if _, err := f.db.Exec(`INSERT INTO album_tags(album_id,tag_id) VALUES('media-album',?)`, created.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("DELETE", path, nil, tag, store.NewID()), 409, "TAG_IN_USE")
	var edges int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM album_tags WHERE tag_id=?`, created.ID).Scan(&edges); err != nil || edges != 1 {
		t.Fatal("assigned-tag rejection cascaded into user album data", edges, err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_tag_receipt BEFORE INSERT ON mutation_receipts BEGIN SELECT RAISE(ABORT,'/private/SQL/receipt'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("PATCH", path, map[string]string{"name": "Must roll back"}, tag, store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(failed.Body.String(), "/private") || strings.Contains(failed.Body.String(), "SQL") || strings.Contains(failed.Body.String(), "receipt") {
		t.Fatal("tag failure disclosed internal SQL/path context", failed.Body.String())
	}
	if value := decodeResponse[access.Tag](t, f.admin.request("GET", path, nil, "", ""), 200); value != created {
		t.Fatal("late receipt failure left a partial tag name/version", value, created)
	}
}
