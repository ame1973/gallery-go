package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
)

// Invoked by all four existing compiled CLI modes after their original 19-file/
// 57-unit repeat/outage assertions. Every source mutation below is test-owned NAS
// simulation; the application only reads these temporary 0400 inputs.
func verifyCompiledExactDuplicates(t *testing.T, api *scanHTTPSClient, source library.Source, marker string, original []byte, workUnits int64) {
	t.Helper()
	if err := os.WriteFile(marker, []byte("isolated scan marker"), 0o400); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(filepath.Dir(marker), "Album", "duplicate-copy.png")
	if err := os.WriteFile(copyPath, original, 0o400); err != nil {
		t.Fatal(err)
	}
	scan := func(mode string) {
		t.Helper()
		var accepted jobs.Job
		api.request("POST", "/admin/scans", map[string]string{"mode": mode, "source_id": source.ID}, 202, &accepted)
		if job := api.waitJob(accepted.ID); job.Status != "succeeded" || job.Done != workUnits+3 || job.Failed != 0 {
			t.Fatal("compiled duplicate input did not publish completely", job, job.Counts)
		}
	}
	scan("full")
	filter := "source_id=" + source.ID + "&library_id=" + source.LibraryID
	groupsPath := "/admin/duplicates/exact?" + filter
	var raw []byte
	headers := api.request("GET", groupsPath, nil, 200, &raw)
	var groups struct {
		Items []media.ExactDuplicateGroup `json:"items"`
	}
	if json.Unmarshal(raw, &groups) != nil || headers.Get("Cache-Control") != "private, no-store" {
		t.Fatal("compiled duplicate group projection was not private", string(raw), headers)
	}
	var group media.ExactDuplicateGroup
	var before []media.Metadata
	for _, candidate := range groups.Items {
		var members struct {
			Items []media.Metadata `json:"items"`
		}
		api.request("GET", "/admin/duplicates/exact/"+candidate.ID+"/media?"+filter, nil, 200, &members)
		if len(members.Items) == 2 && ((members.Items[0].FileName == "photo.png" && members.Items[1].FileName == "duplicate-copy.png") || (members.Items[1].FileName == "photo.png" && members.Items[0].FileName == "duplicate-copy.png")) {
			group, before = candidate, members.Items
			break
		}
	}
	if group.ID == "" || group.MediaCount != 2 || group.SourceSize != int64(len(original)) {
		t.Fatal("native indexed identical bytes did not create an exact group", groups)
	}
	path := "/admin/duplicates/exact/" + group.ID + "/media"
	var page struct {
		Items      []media.Metadata `json:"items"`
		NextCursor *string          `json:"next_cursor"`
		HasMore    bool             `json:"has_more"`
	}
	api.request("GET", path+"?"+filter+"&limit=1", nil, 200, &page)
	if len(page.Items) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatal("compiled duplicate member page was not bounded", page)
	}
	cursor := *page.NextCursor
	api.request("GET", path+"?"+filter+"&limit=1&cursor="+cursor, nil, 200, &page)
	if len(page.Items) != 1 || page.HasMore || page.NextCursor != nil {
		t.Fatal("compiled duplicate member cursor lost its final member", page)
	}
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", groupsPath, nil, 401, nil)
	scan("incremental")
	var replay struct {
		Items []media.Metadata `json:"items"`
	}
	api.request("GET", path+"?"+filter, nil, 200, &replay)
	oldJSON, _ := json.Marshal(before)
	newJSON, _ := json.Marshal(replay.Items)
	if !bytes.Equal(oldJSON, newJSON) {
		t.Fatal("unchanged ingestion replaced duplicate identities/revisions", replay)
	}
	var copied media.Metadata
	for _, item := range before {
		if item.FileName == "duplicate-copy.png" {
			copied = item
		}
	}
	// Simulate an external writer's atomic replacement without making the source
	// writable to the GalleryGo process or deleting any application-owned index.
	picture := image.NewNRGBA(image.Rect(0, 0, 12, 7))
	picture.SetNRGBA(0, 0, color.NRGBA{R: 255, G: 40, A: 255})
	var replacement bytes.Buffer
	if err := png.Encode(&replacement, picture); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(filepath.Dir(filepath.Dir(marker)), "duplicate-replacement-"+source.ID+".png")
	if err := os.WriteFile(replacementPath, replacement.Bytes(), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacementPath, copyPath); err != nil {
		t.Fatal(err)
	}
	scan("full")
	api.request("GET", path+"?"+filter+"&limit=1&cursor="+cursor, nil, 404, nil)
	var changed media.Metadata
	api.request("GET", "/media/"+copied.ID, nil, 200, &changed)
	if copied.Revision == nil || changed.ID != copied.ID || changed.Revision == nil || *changed.Revision == *copied.Revision {
		t.Fatal("same logical copy retained its old duplicate revision after rewrite", copied, changed)
	}
	var downloaded []byte
	api.request("GET", "/media/"+copied.ID+"/original", nil, 200, &downloaded)
	if !bytes.Equal(downloaded, replacement.Bytes()) {
		t.Fatal("indexed copy rewrite did not bind protected original bytes")
	}
	for name, want := range map[string][]byte{copyPath: replacement.Bytes(), filepath.Join(filepath.Dir(marker), "Album", "photo.png"): original} {
		actual, err := os.ReadFile(name)
		info, statErr := os.Stat(name)
		if err != nil || statErr != nil || !bytes.Equal(actual, want) || info.Mode().Perm() != 0o400 {
			t.Fatal("application mutated a read-only duplicate fixture", name, err, statErr)
		}
	}
}
