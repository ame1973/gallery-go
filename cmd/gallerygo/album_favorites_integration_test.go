package main

import (
	"net/http"
	"reflect"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func beginCompiledAlbumFavorite(t *testing.T, api *scanHTTPSClient, id string) (access.AlbumFavorite, http.Header) {
	t.Helper()
	path := "/favorites/" + id
	var initial access.AlbumFavorite
	before := api.request("GET", path, nil, 200, &initial)
	if initial.Favorited || initial.Version != 1 || initial.CollectionID != nil || initial.UpdatedAt != nil {
		t.Fatal("native fixture did not start with a private never-favorited state", initial)
	}
	var albumBefore, albumAfter access.Album
	api.request("GET", "/albums/"+id, nil, 200, &albumBefore)
	headers := http.Header{"If-Match": {before.Get("ETag")}, "Idempotency-Key": {store.NewID()}}
	var changed access.AlbumFavorite
	response := api.request("PUT", path, map[string]any{}, 200, &changed, headers)
	if changed.AlbumID != id || !changed.Favorited || changed.Version != 2 || changed.CollectionID != nil || changed.UpdatedAt == nil {
		t.Fatal("compiled favorite did not publish its private state", changed)
	}
	var noop access.AlbumFavorite
	api.request("PUT", path, map[string]any{"collection_id": nil}, 200, &noop, http.Header{"If-Match": {response.Get("ETag")}})
	api.request("GET", "/albums/"+id, nil, 200, &albumAfter)
	if !reflect.DeepEqual(noop, changed) || !reflect.DeepEqual(albumBefore, albumAfter) {
		t.Fatal("compiled favorite/no-op changed private revision or shared album", noop, changed, albumBefore, albumAfter)
	}
	return changed, headers
}

func finishCompiledAlbumFavorite(t *testing.T, api *scanHTTPSClient, before access.AlbumFavorite, original http.Header) {
	t.Helper()
	path := "/favorites/" + before.AlbumID
	var current access.AlbumFavorite
	currentHeaders := api.request("GET", path, nil, 200, &current)
	if !reflect.DeepEqual(current, before) {
		t.Fatal("actual scan/folder move/metadata/rating edits rewrote a personal favorite", current, before)
	}
	var albumBefore, albumAfter access.Album
	api.request("GET", "/albums/"+before.AlbumID, nil, 200, &albumBefore)
	var cleared access.AlbumFavorite
	clearHeaders := api.request("DELETE", path, nil, 200, &cleared, http.Header{"If-Match": {currentHeaders.Get("ETag")}})
	if cleared.Favorited || cleared.Version != 3 || cleared.CollectionID != nil || cleared.UpdatedAt == nil {
		t.Fatal("compiled favorite clear lost its retained revision", cleared)
	}
	var replay access.AlbumFavorite
	api.request("PUT", path, map[string]any{"collection_id": nil}, 200, &replay, original)
	if !reflect.DeepEqual(replay, before) {
		t.Fatal("native favorite retry was retargeted after scanner changes/clear", replay, before)
	}
	api.request("GET", path, nil, 200, &current)
	if !reflect.DeepEqual(current, cleared) {
		t.Fatal("compiled replay restored a cleared membership", current, cleared)
	}
	api.request("PUT", path, map[string]any{}, 409, nil, http.Header{"If-Match": {original.Get("If-Match")}})
	api.request("DELETE", path, nil, 200, &current, http.Header{"If-Match": {clearHeaders.Get("ETag")}})
	api.request("GET", "/albums/"+before.AlbumID, nil, 200, &albumAfter)
	if !reflect.DeepEqual(current, cleared) || !reflect.DeepEqual(albumBefore, albumAfter) {
		t.Fatal("compiled repeated clear/replay changed a private revision or shared album", current, cleared, albumBefore, albumAfter)
	}
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", path, nil, 401, nil)
}
