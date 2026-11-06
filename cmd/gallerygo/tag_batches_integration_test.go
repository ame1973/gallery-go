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
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func verifyCompiledTagBatchProcessing(t *testing.T, api *scanHTTPSClient, source library.Source, marker string) {
	t.Helper()
	var albums struct {
		Items []access.Album `json:"items"`
	}
	api.request("GET", "/albums", nil, 200, &albums)
	var ids []string
	before := map[string]access.AlbumTagSet{}
	originals := map[string][]media.Metadata{}
	for _, album := range albums.Items {
		if album.LibraryID != source.LibraryID {
			continue
		}
		ids = append(ids, album.ID)
		var tags access.AlbumTagSet
		api.request("GET", "/admin/albums/"+album.ID+"/tags", nil, 200, &tags)
		before[album.ID] = tags
		var items struct {
			Items []media.Metadata `json:"items"`
		}
		api.request("GET", "/albums/"+album.ID+"/media", nil, 200, &items)
		originals[album.ID] = items.Items
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
	tagHeaders := api.request("POST", "/admin/tags", map[string]string{"name": "Processed batch " + store.NewID()}, 201, &tag)
	var currentSource library.Source
	api.request("GET", "/admin/sources/"+source.ID, nil, 200, &currentSource)
	var meBefore, meAfter struct {
		ScopeID string `json:"scope_id"`
	}
	api.request("GET", "/auth/me", nil, 200, &meBefore)
	// A test-owned marker outage must not turn metadata batch work into source IO.
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
	if accepted.Kind != "batch_tags" || accepted.Status != "queued" || accepted.Attempts != 0 || accepted.TotalKnown || accepted.Counts != (jobs.Counts{}) {
		t.Fatal("enqueue itself claimed applied work", accepted)
	}
	processed := api.waitJob(accepted.ID)
	if processed.Status != "succeeded" || processed.Counts != (jobs.Counts{Total: 2, Done: 2, Succeeded: 2}) {
		t.Fatal("actual runtime did not apply both targets", processed, processed.Counts)
	}
	type targetPage struct {
		Items      []access.TagBatchTarget `json:"items"`
		NextCursor *string                 `json:"next_cursor"`
		HasMore    bool                    `json:"has_more"`
	}
	targetPath := "/admin/batches/" + accepted.ID + "/targets"
	var first, last targetPage
	api.request("GET", targetPath+"?limit=1", nil, 200, &first)
	if len(first.Items) != 1 || !first.HasMore || first.NextCursor == nil {
		t.Fatal("compiled target keysets were not bounded", first)
	}
	api.request("GET", targetPath+"?cursor="+*first.NextCursor, nil, 200, &last)
	if len(last.Items) != 1 || last.HasMore || last.NextCursor != nil {
		t.Fatal("compiled target cursor lost selection", last)
	}
	for index, target := range []access.TagBatchTarget{first.Items[0], last.Items[0]} {
		if target.AlbumID != ids[index] || target.AlbumVersion != before[target.AlbumID].Version || target.SourceID == nil || *target.SourceID != source.ID || target.SourceConfigVersion == nil || *target.SourceConfigVersion != currentSource.ConfigVersion || !slices.Equal(target.TagIDs, []string{tag.ID}) {
			t.Fatal("frozen target changed during application", target)
		}
	}
	type resultPage struct {
		Items      []access.AlbumBatchResult `json:"items"`
		NextCursor *string                   `json:"next_cursor"`
		HasMore    bool                      `json:"has_more"`
	}
	resultPath := "/admin/batches/" + accepted.ID + "/results"
	var resultFirst, resultLast resultPage
	api.request("GET", resultPath+"?limit=1", nil, 200, &resultFirst)
	if len(resultFirst.Items) != 1 || !resultFirst.HasMore || resultFirst.NextCursor == nil {
		t.Fatal("compiled result cursor missing", resultFirst)
	}
	api.request("GET", resultPath+"?cursor="+*resultFirst.NextCursor, nil, 200, &resultLast)
	if len(resultLast.Items) != 1 || resultLast.HasMore {
		t.Fatal("compiled result page lost outcome", resultLast)
	}
	resolved := append(resultFirst.Items, resultLast.Items...)
	for _, result := range resolved {
		if result.Outcome != "applied" || result.AlbumVersion == nil || *result.AlbumVersion != before[result.AlbumID].Version+1 || result.ErrorCode != nil {
			t.Fatal("actual result not tied to real target version", result)
		}
	}
	var noop jobs.Job
	api.request("POST", "/admin/batches/tags", input, 202, &noop)
	if noop.ID == accepted.ID {
		t.Fatal("completed snapshot silently swallowed a new command")
	}
	if job := api.waitJob(noop.ID); job.Status != "succeeded" || job.Counts != (jobs.Counts{Total: 2, Done: 2, Skipped: 2}) {
		t.Fatal("identical desired tags invented mutations", job, job.Counts)
	}
	var clear jobs.Job
	api.request("POST", "/admin/batches/tags", access.TagBatchRequest{AlbumIDs: ids, TagIDs: []string{}}, 202, &clear)
	if job := api.waitJob(clear.ID); job.Status != "succeeded" || job.Counts != (jobs.Counts{Total: 2, Done: 2, Succeeded: 2}) {
		t.Fatal("explicit batch clear did not apply", job, job.Counts)
	}
	var historical jobs.Job
	replay := api.request("POST", "/admin/batches/tags", input, 202, &historical, headers)
	if historical.ID != accepted.ID || historical.Status != "queued" || replay.Get("ETag") != response.Get("ETag") {
		t.Fatal("later processing retargeted original enqueue receipt", historical)
	}
	var frozenResults resultPage
	api.request("GET", resultPath, nil, 200, &frozenResults)
	if !reflect.DeepEqual(frozenResults.Items, resolved) {
		t.Fatal("later edits rewrote immutable per-target versions", frozenResults, resolved)
	}
	api.request("DELETE", "/admin/tags/"+tag.ID, nil, 204, nil, http.Header{"If-Match": {tagHeaders.Get("ETag")}})
	for id, initial := range before {
		var tags access.AlbumTagSet
		api.request("GET", "/admin/albums/"+id+"/tags", nil, 200, &tags)
		if tags.Version != initial.Version+2 || len(tags.TagIDs) != 0 {
			t.Fatal("real apply/no-op/clear changed the wrong business versions", tags, initial)
		}
		var items struct {
			Items []media.Metadata `json:"items"`
		}
		api.request("GET", "/albums/"+id+"/media", nil, 200, &items)
		if !reflect.DeepEqual(items.Items, originals[id]) {
			t.Fatal("tag batches changed media identity/revision/metadata", id)
		}
	}
	api.request("GET", "/auth/me", nil, 200, &meAfter)
	if meAfter.ScopeID != meBefore.ScopeID {
		t.Fatal("metadata-only batches rotated authorization scope", meBefore, meAfter)
	}
	for path, digest := range expected {
		data, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil || sha256.Sum256(data) != digest || info.Mode().Perm() != 0o400 {
			t.Fatal("batch processing changed read-only source", path, err, statErr)
		}
	}
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", resultPath, nil, 401, nil)
}
