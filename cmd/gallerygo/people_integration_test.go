package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
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

// This follows the existing four-mode scan/duplicate assertions. It changes only
// an isolated registered source through the real versioned administrative API.
func verifyCompiledPersonMapping(t *testing.T, api *scanHTTPSClient, source library.Source, marker string, workUnits int64) {
	t.Helper()
	headers := api.request("GET", "/admin/sources/"+source.ID, nil, 200, &source)
	body, err := json.Marshal(map[string]any{"mapping": map[string]int{"album_depth": 1, "person_depth": 1}})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), "PATCH", api.origin+"/api/v2/admin/sources/"+source.ID, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", api.origin)
	request.Header.Set("X-CSRF-Token", api.csrf)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", headers.Get("ETag"))
	request.Header.Set("Idempotency-Key", store.NewID())
	response, err := api.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if readErr != nil || response.StatusCode != 200 || json.Unmarshal(data, &source) != nil {
		t.Fatal("compiled person mapping update failed", response.StatusCode, string(data), readErr)
	}
	var accepted jobs.Job
	api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &accepted)
	if job := api.waitJob(accepted.ID); job.Status != "succeeded" || job.Done != workUnits+3 || job.Failed != 0 {
		t.Fatal("compiled source mapping did not publish indexed people", job, job.Counts)
	}
	var people struct {
		Items []access.Person `json:"items"`
	}
	api.request("GET", "/people", nil, 200, &people)
	if len(people.Items) != 1 || people.Items[0].Name != "Album" || people.Items[0].SourceID != source.ID || people.Items[0].AlbumCount != 1 || people.Items[0].CoverMediaID == nil || people.Items[0].Version < 1 {
		t.Fatal("actual configured path mapping did not produce a bounded person DTO", people)
	}
	person := people.Items[0]
	var detail access.Person
	api.request("GET", "/people/"+person.ID, nil, 200, &detail)
	first, _ := json.Marshal(person)
	second, _ := json.Marshal(detail)
	if !bytes.Equal(first, second) {
		t.Fatal("compiled person list/detail visibility differs", person, detail)
	}
	var albums struct {
		Items []access.Album `json:"items"`
	}
	api.request("GET", "/people/"+person.ID+"/albums?limit=1", nil, 200, &albums)
	if len(albums.Items) != 1 || albums.Items[0].MediaCount != (workUnits+3)/3 {
		t.Fatal("compiled person album/count relation lost indexed media", albums)
	}
	api.request("POST", "/admin/scans", map[string]string{"mode": "incremental", "source_id": source.ID}, 202, &accepted)
	if job := api.waitJob(accepted.ID); job.Status != "succeeded" || job.Done != workUnits+3 {
		t.Fatal("compiled person no-op rescan failed", job, job.Counts)
	}
	api.request("GET", "/people/"+person.ID, nil, 200, &detail)
	second, _ = json.Marshal(detail)
	if !bytes.Equal(first, second) {
		t.Fatal("no-op rescan changed indexed person identity/metadata", person, detail)
	}
	anonymous := *api.client
	anonymous.Jar = nil
	guest := &scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}
	for _, path := range []string{"/people", "/people/" + person.ID, "/people/" + person.ID + "/albums"} {
		guest.request("GET", path, nil, 401, nil)
	}
	// Only the test-owned temporary marker is changed. Read endpoints must retain
	// indexed names/counts without probing a source or creating its missing marker.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	api.request("GET", "/people/"+person.ID, nil, 200, &detail)
	second, _ = json.Marshal(detail)
	if !bytes.Equal(first, second) {
		t.Fatal("person read changed its indexed evidence during mount loss", person, detail)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("person endpoint probed/recreated a missing source marker", err)
	}
	if err := os.WriteFile(marker, []byte("isolated scan marker"), 0o400); err != nil {
		t.Fatal(err)
	}
}

func verifyCompiledPersonOverrides(t *testing.T, api *scanHTTPSClient, source library.Source, marker string, workUnits int64) {
	t.Helper()
	var people struct {
		Items []access.Person `json:"items"`
	}
	api.request("GET", "/people", nil, 200, &people)
	if len(people.Items) != 1 {
		t.Fatal("isolated person override fixture lost its single creator", people)
	}
	personID := people.Items[0].ID
	var albums struct {
		Items []access.Album `json:"items"`
	}
	api.request("GET", "/people/"+personID+"/albums", nil, 200, &albums)
	if len(albums.Items) != 1 {
		t.Fatal("person override fixture lost its indexed album", albums)
	}
	albumID := albums.Items[0].ID
	var items struct {
		Items []media.Metadata `json:"items"`
	}
	api.request("GET", "/albums/"+albumID+"/media", nil, 200, &items)
	root := filepath.Dir(marker)
	expected := make(map[string][32]byte)
	for _, item := range items.Items {
		data, err := os.ReadFile(filepath.Join(root, "RenamedAlbum", item.FileName))
		if err != nil {
			t.Fatal(err)
		}
		expected[item.FileName] = sha256.Sum256(data)
	}
	path := "/admin/people/" + personID
	var before access.PersonMetadata
	initial := api.request("GET", path, nil, 200, &before)
	if before.SourceName != "RenamedAlbum" || before.SourceID != source.ID || before.DisplayName != nil {
		t.Fatal("person override fixture does not follow the prior native folder move", before)
	}
	patch := map[string]string{"display_name": "Persisted creator override"}
	headers := http.Header{"If-Match": {initial.Get("ETag")}, "Idempotency-Key": {store.NewID()}}
	var changed access.PersonMetadata
	changedHeaders := api.request("PATCH", path, patch, 200, &changed, headers)
	if changed.Version != before.Version+1 || changed.DisplayName == nil || *changed.DisplayName != patch["display_name"] || changed.SourceID != before.SourceID || changed.SourceName != before.SourceName {
		t.Fatal("compiled person override did not preserve its source-bound metadata", before, changed)
	}
	scan := func() {
		t.Helper()
		var accepted jobs.Job
		api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &accepted)
		if job := api.waitJob(accepted.ID); job.Status != "succeeded" || job.Done != workUnits+3 || job.Failed != 0 {
			t.Fatal("scan after person override failed", job, job.Counts)
		}
	}
	scan()
	var stable access.PersonMetadata
	api.request("GET", path, nil, 200, &stable)
	if !reflect.DeepEqual(stable, changed) {
		t.Fatal("no-op scan changed the manual person label/version", stable, changed)
	}
	// Simulate an external whole-folder move of only the test-owned 0400 files.
	// Source paths change, but logical identities and manual labels must survive.
	if err := os.Rename(filepath.Join(root, "RenamedAlbum"), filepath.Join(root, "RenamedCreator")); err != nil {
		t.Fatal(err)
	}
	scan()
	latest := api.request("GET", path, nil, 200, &stable)
	if stable.ID != personID || stable.SourceID != source.ID || stable.SourceName != "RenamedCreator" || stable.DisplayName == nil || *stable.DisplayName != patch["display_name"] || stable.Version <= changed.Version {
		t.Fatal("provable person move lost its identity or manual label", stable, changed)
	}
	var public access.Person
	api.request("GET", "/people/"+personID, nil, 200, &public)
	if public.Name != patch["display_name"] || public.Version != stable.Version || public.AlbumCount != 1 {
		t.Fatal("public person projection lost the retained display override", public)
	}
	var replay access.PersonMetadata
	replayedHeaders := api.request("PATCH", path, patch, 200, &replay, headers)
	if !reflect.DeepEqual(replay, changed) || replayedHeaders.Get("ETag") != changedHeaders.Get("ETag") || replayedHeaders.Get("ETag") == latest.Get("ETag") {
		t.Fatal("historical person receipt was retargeted by a native scan", replay, changed)
	}
	var cleared access.PersonMetadata
	api.request("PATCH", path, map[string]string{"display_name": ""}, 200, &cleared, http.Header{"If-Match": {latest.Get("ETag")}})
	if cleared.DisplayName != nil || cleared.SourceName != "RenamedCreator" || cleared.Version != stable.Version+1 {
		t.Fatal("clear did not restore the current indexed creator label", cleared)
	}
	api.request("GET", "/people/"+personID, nil, 200, &public)
	api.request("GET", "/people/"+personID+"/albums", nil, 200, &albums)
	if public.Name != "RenamedCreator" || len(albums.Items) != 1 || albums.Items[0].ID != albumID {
		t.Fatal("person metadata command reassigned its album or source label", public, albums)
	}
	var current struct {
		Items []media.Metadata `json:"items"`
	}
	api.request("GET", "/albums/"+albumID+"/media", nil, 200, &current)
	if len(current.Items) != len(items.Items) {
		t.Fatal("person folder move changed indexed media count", len(current.Items), len(items.Items))
	}
	for index, item := range items.Items {
		path := filepath.Join(root, "RenamedCreator", item.FileName)
		info, err := os.Stat(path)
		data, readErr := os.ReadFile(path)
		if err != nil || readErr != nil || info.Mode().Perm() != 0o400 || sha256.Sum256(data) != expected[item.FileName] || current.Items[index].ID != item.ID {
			t.Fatal("person override/move changed source bytes/mode or logical media ID", item.ID, err, readErr)
		}
	}
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", path, nil, 401, nil)
}
