package main

import (
	"net/http"
	"reflect"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func beginCompiledAlbumView(t *testing.T, api *scanHTTPSClient, id string) (access.AlbumView, http.Header) {
	t.Helper()
	path := "/albums/" + id + "/views"
	var initial access.AlbumView
	api.request("GET", path, nil, 200, &initial)
	if initial.ViewCount != 0 || initial.Version != 1 || initial.LastViewedAt != nil {
		t.Fatal("native fixture did not start with a never-viewed personal state", initial)
	}
	headers := http.Header{"Idempotency-Key": {store.NewID()}}
	var recorded, replay access.AlbumView
	api.request("POST", path, nil, 200, &recorded, headers)
	api.request("POST", path, nil, 200, &replay, headers)
	if recorded.ViewCount != 1 || recorded.Version != 2 || recorded.LastViewedAt == nil || !reflect.DeepEqual(replay, recorded) {
		t.Fatal("native first view or its actor receipt was not stable", recorded, replay)
	}
	var album access.Album
	api.request("GET", "/albums/"+id, nil, 200, &album)
	if album.ViewCount != 1 {
		t.Fatal("native shared album lost the view total", album)
	}
	return recorded, headers
}

func finishCompiledAlbumView(t *testing.T, api *scanHTTPSClient, before access.AlbumView, original http.Header) {
	t.Helper()
	path := "/albums/" + before.AlbumID + "/views"
	var current, replay access.AlbumView
	api.request("GET", path, nil, 200, &current)
	api.request("POST", path, nil, 200, &replay, original)
	if !reflect.DeepEqual(current, before) || !reflect.DeepEqual(replay, before) {
		t.Fatal("native scans/folder move changed personal views or historical replay", current, replay, before)
	}
	api.request("POST", path, nil, 200, &current)
	if current.ViewCount != 2 || current.Version != 3 || current.LastViewedAt == nil || current.LastViewedAt.Before(*before.LastViewedAt) {
		t.Fatal("native second view did not advance monotonic personal state", current)
	}
	var album access.Album
	api.request("GET", "/albums/"+before.AlbumID, nil, 200, &album)
	if album.ViewCount != 2 {
		t.Fatal("native scan/folder move lost the shared view total", album)
	}
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", path, nil, 401, nil)
}
