package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
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
