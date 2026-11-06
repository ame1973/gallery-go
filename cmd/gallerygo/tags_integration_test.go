package main

import (
	"net/http"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func verifyCompiledTagCatalog(t *testing.T, api *scanHTTPSClient) {
	t.Helper()
	var me auth.User
	api.request("GET", "/auth/me", nil, 200, &me)
	createKey := store.NewID()
	var created access.Tag
	initial := api.request("POST", "/admin/tags", map[string]string{"name": " CLI catalog tag "}, 201, &created, http.Header{"Idempotency-Key": {createKey}})
	if created.Name != "CLI catalog tag" || created.Version != 1 || initial.Get("Location") != "/api/v2/admin/tags/"+created.ID || initial.Get("ETag") == "" {
		t.Fatal("compiled tag creation did not expose exact versioned metadata", created, initial)
	}
	path := "/admin/tags/" + created.ID
	renameHeaders := http.Header{"If-Match": {initial.Get("ETag")}, "Idempotency-Key": {store.NewID()}}
	var changed access.Tag
	current := api.request("PATCH", path, map[string]string{"name": "CLI renamed tag"}, 200, &changed, renameHeaders)
	if changed.ID != created.ID || changed.Name != "CLI renamed tag" || changed.Version != 2 {
		t.Fatal("compiled rename lost tag identity/version", created, changed)
	}
	var noop access.Tag
	api.request("PATCH", path, map[string]string{"name": " CLI renamed tag "}, 200, &noop, http.Header{"If-Match": {current.Get("ETag")}})
	if noop != changed {
		t.Fatal("compiled same-name command was not a version no-op", noop, changed)
	}
	api.request("POST", "/admin/tags", map[string]string{"name": "CLI renamed tag"}, 409, nil)
	var historical access.Tag
	replayed := api.request("POST", "/admin/tags", map[string]string{"name": "CLI catalog tag"}, 201, &historical, http.Header{"Idempotency-Key": {createKey}})
	if historical != created || replayed.Get("ETag") != initial.Get("ETag") {
		t.Fatal("compiled creation replay was retargeted by rename", historical, created)
	}
	api.request("GET", path, nil, 200, &historical)
	if historical != changed {
		t.Fatal("current tag GET did not distinguish historical receipts", historical, changed)
	}
	api.request("DELETE", path, nil, 409, nil, http.Header{"If-Match": {initial.Get("ETag")}})
	deleteHeaders := http.Header{"If-Match": {current.Get("ETag")}, "Idempotency-Key": {store.NewID()}}
	for range 2 {
		api.request("DELETE", path, nil, 204, nil, deleteHeaders)
	}
	api.request("GET", path, nil, 404, nil)
	api.request("PATCH", path, map[string]string{"name": "CLI renamed tag"}, 200, &historical, renameHeaders)
	if historical != changed {
		t.Fatal("compiled rename receipt lost history after deletion", historical, changed)
	}
	var after auth.User
	api.request("GET", "/auth/me", nil, 200, &after)
	if after.ScopeID != me.ScopeID {
		t.Fatal("tag catalog commands changed authorization scope", me.ScopeID, after.ScopeID)
	}
	anonymous := *api.client
	anonymous.Jar = nil
	guest := &scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}
	guest.request("GET", "/admin/tags", nil, 401, nil)
	guest.request("GET", path, nil, 401, nil)
}
