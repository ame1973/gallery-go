package main

import (
	"crypto/sha256"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/store"
)

func verifyCompiledTagBatchAcceptance(t *testing.T, api *scanHTTPSClient, source library.Source, marker string) {
	t.Helper()
	var albums struct {
		Items []access.Album `json:"items"`
	}
	api.request("GET", "/albums", nil, 200, &albums)
	var ids []string
	before := map[string]access.AlbumTagSet{}
	for _, album := range albums.Items {
		if album.LibraryID != source.LibraryID {
			continue
		}
		ids = append(ids, album.ID)
		var tags access.AlbumTagSet
		api.request("GET", "/admin/albums/"+album.ID+"/tags", nil, 200, &tags)
		before[album.ID] = tags
	}
	if len(ids) != 2 {
		t.Fatal("batch fixture lost its two natively indexed albums", ids)
	}
	slices.Sort(ids)
	root := filepath.Dir(marker)
	expected := map[string][32]byte{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || path == marker {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		expected[path] = sha256.Sum256(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(expected) != 21 {
		t.Fatal("batch fixture changed its original media set", len(expected))
	}
	var tag access.Tag
	tagHeaders := api.request("POST", "/admin/tags", map[string]string{"name": "Queued batch " + store.NewID()}, 201, &tag)
	var currentSource library.Source
	sourceHeaders := api.request("GET", "/admin/sources/"+source.ID, nil, 200, &currentSource)
	// This is a test-owned mount-marker outage, not a source writer. Acceptance
	// and historical target queries must work without touching that mount.
	offline := marker + "-batch-offline"
	if err := os.Rename(marker, offline); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(offline, marker); err != nil {
			t.Error(err)
		}
	}()
	input := access.TagBatchRequest{AlbumIDs: ids, TagIDs: []string{tag.ID}}
	headers := http.Header{"Idempotency-Key": {store.NewID()}}
	var accepted jobs.Job
	response := api.request("POST", "/admin/batches/tags", input, 202, &accepted, headers)
	if accepted.Kind != "batch_tags" || accepted.Status != "queued" || accepted.Attempts != 0 || accepted.TotalKnown || accepted.Counts != (jobs.Counts{}) || response.Get("Location") != "/api/v2/admin/jobs/"+accepted.ID {
		t.Fatal("compiled enqueue claimed applied metadata", accepted)
	}
	var coalesced jobs.Job
	api.request("POST", "/admin/batches/tags", input, 202, &coalesced)
	if coalesced.ID != accepted.ID {
		t.Fatal("compiled identical frozen intent did not coalesce", coalesced)
	}
	targetPath := "/admin/batches/" + accepted.ID + "/targets"
	type targetPage struct {
		Items      []access.TagBatchTarget `json:"items"`
		NextCursor *string                 `json:"next_cursor"`
		HasMore    bool                    `json:"has_more"`
	}
	var first, last targetPage
	api.request("GET", targetPath+"?limit=1", nil, 200, &first)
	if len(first.Items) != 1 || !first.HasMore || first.NextCursor == nil {
		t.Fatal("compiled target keyset was not bounded", first)
	}
	api.request("GET", targetPath+"?cursor="+*first.NextCursor, nil, 200, &last)
	if len(last.Items) != 1 || last.HasMore || last.NextCursor != nil {
		t.Fatal("compiled target cursor lost frozen selection", last)
	}
	for index, target := range []access.TagBatchTarget{first.Items[0], last.Items[0]} {
		if target.AlbumID != ids[index] || target.AlbumVersion != before[target.AlbumID].Version || target.SourceID == nil || *target.SourceID != source.ID || target.SourceConfigVersion == nil || *target.SourceConfigVersion != currentSource.ConfigVersion || !slices.Equal(target.TagIDs, []string{tag.ID}) {
			t.Fatal("compiled snapshot lost resource/config epochs", target)
		}
	}
	var changedSource library.Source
	api.request("PATCH", "/admin/sources/"+source.ID, map[string]any{"minimum_files": currentSource.MinimumFiles + 1}, 200, &changedSource, http.Header{"If-Match": {sourceHeaders.Get("ETag")}})
	var current jobs.Job
	api.request("GET", "/admin/jobs/"+accepted.ID, nil, 200, &current)
	if current.Status != "cancelled" || current.Attempts != 0 || current.Succeeded != 0 {
		t.Fatal("source epoch did not cancel bound batch", current)
	}
	var historical jobs.Job
	replay := api.request("POST", "/admin/batches/tags", input, 202, &historical, headers)
	if historical.ID != accepted.ID || historical.Status != "queued" || replay.Get("ETag") != response.Get("ETag") {
		t.Fatal("compiled receipt lost original accepted state", historical)
	}
	var frozen targetPage
	api.request("GET", targetPath, nil, 200, &frozen)
	if len(frozen.Items) != 2 || !reflect.DeepEqual(frozen.Items, []access.TagBatchTarget{first.Items[0], last.Items[0]}) {
		t.Fatal("configuration edit rewrote frozen target history", frozen)
	}
	var next jobs.Job
	nextHeaders := api.request("POST", "/admin/batches/tags", input, 202, &next)
	if next.ID == accepted.ID {
		t.Fatal("new source configuration reused old intent", next)
	}
	var cancelled jobs.Job
	api.request("POST", "/admin/jobs/"+next.ID+"/cancel", nil, 200, &cancelled, http.Header{"If-Match": {nextHeaders.Get("ETag")}})
	if cancelled.Status != "cancelled" || cancelled.Succeeded != 0 {
		t.Fatal("queued batch cancellation invented completed work", cancelled)
	}
	api.request("DELETE", "/admin/tags/"+tag.ID, nil, 204, nil, http.Header{"If-Match": {tagHeaders.Get("ETag")}})
	for id, initial := range before {
		var tags access.AlbumTagSet
		api.request("GET", "/admin/albums/"+id+"/tags", nil, 200, &tags)
		if !reflect.DeepEqual(tags, initial) {
			t.Fatal("queue/cancel changed actual metadata", tags, initial)
		}
	}
	for path, digest := range expected {
		data, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil || sha256.Sum256(data) != digest || info.Mode().Perm() != 0o400 {
			t.Fatal("batch control changed read-only source", path, err, statErr)
		}
	}
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", targetPath, nil, 401, nil)
}
