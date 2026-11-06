package main

import (
	"crypto/sha256"
	"io/fs"
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

func verifyCompiledCoverBatches(t *testing.T, api *scanHTTPSClient, source library.Source, marker string, workUnits int64) {
	t.Helper()
	var albums struct {
		Items []access.Album `json:"items"`
	}
	api.request("GET", "/albums", nil, 200, &albums)
	before := map[string]access.AlbumMetadata{}
	originals := map[string][]media.Metadata{}
	input := access.CoverBatchRequest{Targets: []access.CoverBatchSelection{}}
	for _, album := range albums.Items {
		if album.LibraryID != source.LibraryID {
			continue
		}
		var state access.AlbumMetadata
		api.request("GET", "/admin/albums/"+album.ID, nil, 200, &state)
		before[album.ID] = state
		var items struct {
			Items []media.Metadata `json:"items"`
		}
		api.request("GET", "/albums/"+album.ID+"/media", nil, 200, &items)
		originals[album.ID] = items.Items
		requested := ""
		for _, item := range items.Items {
			if state.CoverOverrideID == nil || *state.CoverOverrideID != item.ID {
				requested = item.ID
				break
			}
		}
		if requested == "" && state.CoverOverrideID == nil {
			t.Fatal("native album has no eligible cover", album.ID)
		}
		input.Targets = append(input.Targets, access.CoverBatchSelection{AlbumID: album.ID, CoverMediaID: &requested})
	}
	if len(input.Targets) != 2 {
		t.Fatal("cover fixture lost native albums", input)
	}
	expected := map[string][32]byte{}
	root := filepath.Dir(marker)
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
		t.Fatal("cover fixture changed media set", len(expected))
	}
	offline := marker + "-cover-offline"
	if err := os.Rename(marker, offline); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := os.Stat(offline); err == nil {
			if err := os.Rename(offline, marker); err != nil {
				t.Error(err)
			}
		}
	}()
	headers := http.Header{"Idempotency-Key": {store.NewID()}}
	var accepted jobs.Job
	response := api.request("POST", "/admin/batches/covers", input, 202, &accepted, headers)
	if accepted.Kind != "batch_covers" || accepted.Status != "queued" || accepted.TotalKnown || accepted.Done != 0 {
		t.Fatal("cover enqueue claimed work", accepted)
	}
	if result := api.waitJob(accepted.ID); result.Status != "succeeded" || result.Counts != (jobs.Counts{Total: 2, Done: 2, Succeeded: 2}) {
		t.Fatal("actual cover worker did not apply both preferences", result, result.Counts)
	}
	type targetPage struct {
		Items      []access.CoverBatchTarget `json:"items"`
		NextCursor *string                   `json:"next_cursor"`
		HasMore    bool                      `json:"has_more"`
	}
	path := "/admin/batches/" + accepted.ID + "/covers"
	var first, last targetPage
	api.request("GET", path+"?limit=1", nil, 200, &first)
	if len(first.Items) != 1 || first.NextCursor == nil || !first.HasMore {
		t.Fatal("cover target keysets not bounded", first)
	}
	api.request("GET", path+"?cursor="+*first.NextCursor, nil, 200, &last)
	if len(last.Items) != 1 || last.HasMore {
		t.Fatal("cover target cursor lost preference", last)
	}
	frozen := append(first.Items, last.Items...)
	changed := map[string]access.AlbumMetadata{}
	for _, selection := range input.Targets {
		var state access.AlbumMetadata
		api.request("GET", "/admin/albums/"+selection.AlbumID, nil, 200, &state)
		changed[selection.AlbumID] = state
		if state.Version != before[selection.AlbumID].Version+1 || !sameCompiledCover(state.CoverOverrideID, *selection.CoverMediaID) || !reflect.DeepEqual(state.DisplayName, before[selection.AlbumID].DisplayName) {
			t.Fatal("cover mutation changed unrelated metadata", state, before[selection.AlbumID])
		}
	}
	var noop jobs.Job
	api.request("POST", "/admin/batches/covers", input, 202, &noop)
	if result := api.waitJob(noop.ID); result.Status != "succeeded" || result.Counts != (jobs.Counts{Total: 2, Done: 2, Skipped: 2}) {
		t.Fatal("identical cover preferences invented changes", result, result.Counts)
	}
	if err := os.Rename(offline, marker); err != nil {
		t.Fatal(err)
	}
	var scan jobs.Job
	api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &scan)
	if result := api.waitJob(scan.ID); result.Status != "succeeded" || result.Done != workUnits+6 || result.Failed != 0 {
		t.Fatal("native cover-retention scan failed", result, result.Counts)
	}
	clear := access.CoverBatchRequest{Targets: []access.CoverBatchSelection{}}
	var clearChanges int64
	for _, selection := range input.Targets {
		var stable access.AlbumMetadata
		api.request("GET", "/admin/albums/"+selection.AlbumID, nil, 200, &stable)
		if !reflect.DeepEqual(stable, changed[selection.AlbumID]) {
			t.Fatal("native no-op scan rewrote batch cover preference", stable, changed[selection.AlbumID])
		}
		clear.Targets = append(clear.Targets, access.CoverBatchSelection{AlbumID: selection.AlbumID, CoverMediaID: new("")})
		if stable.CoverOverrideID != nil {
			clearChanges++
		}
	}
	var clearJob jobs.Job
	api.request("POST", "/admin/batches/covers", clear, 202, &clearJob)
	if result := api.waitJob(clearJob.ID); result.Status != "succeeded" || result.Counts != (jobs.Counts{Total: 2, Done: 2, Succeeded: clearChanges, Skipped: 2 - clearChanges}) {
		t.Fatal("cover clear/no-op counts not truthful", result, result.Counts)
	}
	var historical jobs.Job
	replayed := api.request("POST", "/admin/batches/covers", input, 202, &historical, headers)
	if historical.ID != accepted.ID || historical.Status != "queued" || replayed.Get("ETag") != response.Get("ETag") {
		t.Fatal("cover processing retargeted historical acceptance", historical)
	}
	var history targetPage
	api.request("GET", path, nil, 200, &history)
	if !reflect.DeepEqual(history.Items, frozen) {
		t.Fatal("cover target history changed after clear", history.Items, frozen)
	}
	var outcomes struct {
		Items []access.AlbumBatchResult `json:"items"`
	}
	api.request("GET", "/admin/batches/"+accepted.ID+"/results", nil, 200, &outcomes)
	if len(outcomes.Items) != 2 {
		t.Fatal("cover result history missing", outcomes)
	}
	for _, outcome := range outcomes.Items {
		if outcome.Outcome != "applied" || outcome.AlbumVersion == nil || *outcome.AlbumVersion != changed[outcome.AlbumID].Version {
			t.Fatal("cover result version is not immutable", outcome)
		}
	}
	for id, previous := range changed {
		var state access.AlbumMetadata
		api.request("GET", "/admin/albums/"+id, nil, 200, &state)
		if state.CoverOverrideID != nil || !reflect.DeepEqual(state.CoverMediaID, previous.CoverMediaID) || !reflect.DeepEqual(state.DisplayName, previous.DisplayName) {
			t.Fatal("clear lost sticky current cover/other preference", state, previous)
		}
		var items struct {
			Items []media.Metadata `json:"items"`
		}
		api.request("GET", "/albums/"+id+"/media", nil, 200, &items)
		if !reflect.DeepEqual(items.Items, originals[id]) {
			t.Fatal("cover batches changed media identity/revision/metadata", id)
		}
	}
	for path, digest := range expected {
		data, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil || sha256.Sum256(data) != digest || info.Mode().Perm() != 0o400 {
			t.Fatal("cover batch/scan wrote source", path, err, statErr)
		}
	}
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", path, nil, 401, nil)
}

func sameCompiledCover(actual *string, requested string) bool {
	return requested == "" && actual == nil || actual != nil && *actual == requested
}
