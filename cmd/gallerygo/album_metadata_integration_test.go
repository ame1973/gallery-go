package main

import (
	"crypto/sha256"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func verifyCompiledAlbumOverrides(t *testing.T, api *scanHTTPSClient, source library.Source, marker string, workUnits int64) {
	t.Helper()
	var albums struct {
		Items []access.Album `json:"items"`
	}
	api.request("GET", "/albums", nil, 200, &albums)
	if len(albums.Items) != 1 {
		t.Fatal("isolated album override fixture lost its single indexed album", albums)
	}
	albumID := albums.Items[0].ID
	var items struct {
		Items []media.Metadata `json:"items"`
	}
	api.request("GET", "/albums/"+albumID+"/media", nil, 200, &items)
	root := filepath.Dir(marker)
	expected := make(map[string][32]byte)
	var selected string
	for _, item := range items.Items {
		data, err := os.ReadFile(filepath.Join(root, "Album", item.FileName))
		if err != nil {
			t.Fatal(err)
		}
		expected[item.FileName] = sha256.Sum256(data)
		if item.FileName == "photo.png" {
			selected = item.ID
		}
	}
	if selected == "" {
		t.Fatal("compiled album cover fixture is missing")
	}
	path := "/admin/albums/" + albumID
	var before access.AlbumMetadata
	initial := api.request("GET", path, nil, 200, &before)
	key := store.NewID()
	patch := map[string]string{"display_name": "Persisted display", "cover_media_id": selected}
	headers := http.Header{"If-Match": {initial.Get("ETag")}, "Idempotency-Key": {key}}
	var changed access.AlbumMetadata
	api.request("PATCH", path, patch, 200, &changed, headers)
	if changed.Version != before.Version+1 || changed.DisplayName == nil || *changed.DisplayName != "Persisted display" || changed.CoverOverrideID == nil || *changed.CoverOverrideID != selected || changed.CoverMediaID == nil || *changed.CoverMediaID != selected {
		t.Fatal("compiled versioned override did not persist its metadata", before, changed)
	}
	assertCompiledAlbumOrder(t, api, source.ID, 1)
	scan := func() {
		t.Helper()
		var accepted jobs.Job
		api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &accepted)
		if job := api.waitJob(accepted.ID); job.Status != "succeeded" || job.Done != workUnits+3 || job.Failed != 0 {
			t.Fatal("scan after album metadata update failed", job, job.Counts)
		}
	}
	scan()
	var stable access.AlbumMetadata
	api.request("GET", path, nil, 200, &stable)
	if !reflect.DeepEqual(stable, changed) {
		t.Fatal("unchanged scan overwrote display/cover preference or version", stable, changed)
	}
	assertCompiledAlbumOrder(t, api, source.ID, 1)
	// Simulate only an external rename of this test's own temporary 0400 source
	// folder. The application must preserve logical album/person/media identities.
	if err := os.Rename(filepath.Join(root, "Album"), filepath.Join(root, "RenamedAlbum")); err != nil {
		t.Fatal(err)
	}
	scan()
	latest := api.request("GET", path, nil, 200, &stable)
	if stable.ID != albumID || stable.SourceName != "RenamedAlbum" || stable.DisplayName == nil || *stable.DisplayName != "Persisted display" || stable.CoverOverrideID == nil || *stable.CoverOverrideID != selected || stable.CoverMediaID == nil || *stable.CoverMediaID != selected || stable.Version <= changed.Version {
		t.Fatal("provable folder move lost immutable identity or manual overrides", stable, changed)
	}
	assertCompiledAlbumOrder(t, api, source.ID, 1)
	var replay access.AlbumMetadata
	replayedHeaders := api.request("PATCH", path, patch, 200, &replay, headers)
	if !reflect.DeepEqual(replay, changed) || replayedHeaders.Get("ETag") == latest.Get("ETag") {
		t.Fatal("receipt was not the historical result after scanner metadata updates", replay, changed)
	}
	var cleared access.AlbumMetadata
	api.request("PATCH", path, map[string]string{"display_name": "", "cover_media_id": ""}, 200, &cleared, http.Header{"If-Match": {latest.Get("ETag")}})
	if cleared.DisplayName != nil || cleared.CoverOverrideID != nil || cleared.CoverMediaID == nil || *cleared.CoverMediaID != selected || cleared.SourceName != "RenamedAlbum" {
		t.Fatal("clear command did not return to sticky automatic/source display", cleared)
	}
	var public access.Album
	api.request("GET", "/albums/"+albumID, nil, 200, &public)
	if public.Title != "RenamedAlbum" || public.CoverMediaID == nil || *public.CoverMediaID != selected {
		t.Fatal("current public album projection diverged from cleared preferences", public)
	}
	assertCompiledAlbumOrder(t, api, source.ID, 1)
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", path, nil, 401, nil)
	for _, item := range items.Items {
		path := filepath.Join(root, "RenamedAlbum", item.FileName)
		info, err := os.Stat(path)
		data, readErr := os.ReadFile(path)
		if err != nil || readErr != nil || info.Mode().Perm() != 0o400 || sha256.Sum256(data) != expected[item.FileName] {
			t.Fatal("album override mutation changed source bytes/mode", item.FileName, err, readErr)
		}
	}
}
