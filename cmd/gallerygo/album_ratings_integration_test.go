package main

import (
	"net/http"
	"reflect"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func beginCompiledAlbumRating(t *testing.T, api *scanHTTPSClient, id string) (access.AlbumRating, http.Header) {
	t.Helper()
	path := "/albums/" + id + "/rating"
	var initial access.AlbumRating
	before := api.request("GET", path, nil, 200, &initial)
	if initial.Rating != nil || initial.Version != 1 || initial.UpdatedAt != nil {
		t.Fatal("native fixture did not start with a private never-rated state", initial)
	}
	headers := http.Header{"If-Match": {before.Get("ETag")}, "Idempotency-Key": {store.NewID()}}
	var changed access.AlbumRating
	response := api.request("PUT", path, map[string]int{"rating": 5}, 200, &changed, headers)
	if changed.AlbumID != id || changed.Version != 2 || changed.Rating == nil || *changed.Rating != 5 || changed.UpdatedAt == nil {
		t.Fatal("compiled rating did not publish its real personal state", changed)
	}
	var noop access.AlbumRating
	api.request("PUT", path, map[string]int{"rating": 5}, 200, &noop, http.Header{"If-Match": {response.Get("ETag")}})
	if !reflect.DeepEqual(noop, changed) {
		t.Fatal("compiled same-vote replacement invented a new revision", noop, changed)
	}
	return changed, headers
}

func finishCompiledAlbumRating(t *testing.T, api *scanHTTPSClient, before access.AlbumRating, original http.Header) {
	t.Helper()
	path := "/albums/" + before.AlbumID + "/rating"
	var current access.AlbumRating
	currentHeaders := api.request("GET", path, nil, 200, &current)
	if !reflect.DeepEqual(current, before) {
		t.Fatal("actual scan/folder move/metadata edit rewrote a personal vote", before, current)
	}
	var album access.Album
	api.request("GET", "/albums/"+before.AlbumID, nil, 200, &album)
	if album.RatingAvg == nil || *album.RatingAvg != 5 {
		t.Fatal("native publication lost the live rating aggregate", album)
	}
	var cleared access.AlbumRating
	clearHeaders := api.request("DELETE", path, nil, 200, &cleared, http.Header{"If-Match": {currentHeaders.Get("ETag")}})
	if cleared.Version != 3 || cleared.Rating != nil || cleared.UpdatedAt == nil {
		t.Fatal("compiled clear lost its retained revision", cleared)
	}
	var replay access.AlbumRating
	api.request("PUT", path, map[string]int{"rating": 5}, 200, &replay, original)
	if !reflect.DeepEqual(replay, before) {
		t.Fatal("native retry was retargeted after scanner changes and clear", replay, before)
	}
	api.request("GET", path, nil, 200, &current)
	if !reflect.DeepEqual(current, cleared) {
		t.Fatal("compiled replay restored a removed vote", current, cleared)
	}
	api.request("PUT", path, map[string]int{"rating": 5}, 409, nil, http.Header{"If-Match": {original.Get("If-Match")}})
	api.request("DELETE", path, nil, 200, &current, http.Header{"If-Match": {clearHeaders.Get("ETag")}})
	if !reflect.DeepEqual(current, cleared) {
		t.Fatal("compiled repeated clear rewrote its tombstone", current, cleared)
	}
	api.request("GET", "/albums/"+before.AlbumID, nil, 200, &album)
	if album.RatingAvg != nil {
		t.Fatal("compiled cleared vote remained in the shared average", album)
	}
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", path, nil, 401, nil)
}
