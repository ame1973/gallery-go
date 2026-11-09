package main

import (
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

type compiledFavoriteCollections struct {
	original access.FavoriteCollection
	renamed  access.FavoriteCollection
	child    access.FavoriteCollection
	key      string
}

func beginCompiledFavoriteCollections(t *testing.T, api *scanHTTPSClient) compiledFavoriteCollections {
	t.Helper()
	var value compiledFavoriteCollections
	value.key = store.NewID()
	created := api.request("POST", "/favorite-collections", map[string]string{"name": "Native root"}, 201, &value.original, http.Header{"Idempotency-Key": {value.key}})
	if value.original.ParentID != nil || value.original.Version != 1 || value.original.Name != "Native root" {
		t.Fatal("compiled root collection was not created", value.original)
	}
	api.request("POST", "/favorite-collections", map[string]string{"name": "Native child", "parent_id": value.original.ID}, 201, &value.child)
	if value.child.ParentID == nil || *value.child.ParentID != value.original.ID || value.child.Version != 1 {
		t.Fatal("compiled child collection lost its owned parent", value.child)
	}
	api.request("PATCH", "/favorite-collections/"+value.original.ID, map[string]string{"name": "Native renamed"}, 200, &value.renamed, http.Header{"If-Match": {created.Get("ETag")}})
	if value.renamed.ID != value.original.ID || value.renamed.Name != "Native renamed" || value.renamed.Version != 2 {
		t.Fatal("compiled collection rename lost its identity/revision", value.renamed)
	}
	return value
}

func finishCompiledFavoriteCollections(t *testing.T, api *scanHTTPSClient, before compiledFavoriteCollections) {
	t.Helper()
	var root, child access.FavoriteCollection
	api.request("GET", "/favorite-collections/"+before.original.ID, nil, 200, &root)
	api.request("GET", "/favorite-collections/"+before.child.ID, nil, 200, &child)
	if !reflect.DeepEqual(root, before.renamed) || !reflect.DeepEqual(child, before.child) {
		t.Fatal("native scans changed owned collection metadata", root, child, before)
	}
	var page struct {
		Items      []access.FavoriteCollection `json:"items"`
		NextCursor *string                     `json:"next_cursor"`
		HasMore    bool                        `json:"has_more"`
	}
	api.request("GET", "/favorite-collections?parent_id="+url.QueryEscape(root.ID), nil, 200, &page)
	if len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], child) || page.HasMore || page.NextCursor != nil {
		t.Fatal("native owned collection page lost its child", page)
	}
	var replay access.FavoriteCollection
	api.request("POST", "/favorite-collections", map[string]any{"name": "Native root", "parent_id": nil}, 201, &replay, http.Header{"Idempotency-Key": {before.key}})
	if !reflect.DeepEqual(replay, before.original) {
		t.Fatal("compiled create receipt was not historical", replay, before.original)
	}
	api.request("GET", "/favorite-collections/"+root.ID, nil, 200, &root)
	if !reflect.DeepEqual(root, before.renamed) {
		t.Fatal("compiled create replay restored an obsolete collection name", root, before.renamed)
	}
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", "/favorite-collections", nil, 401, nil)
}
