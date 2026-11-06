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

func verifyCompiledCreatorOverrides(t *testing.T, api *scanHTTPSClient, source library.Source, marker string, workUnits int64) {
	t.Helper()
	var albums struct {
		Items []access.Album `json:"items"`
	}
	api.request("GET", "/albums", nil, 200, &albums)
	if len(albums.Items) != 1 {
		t.Fatal("creator fixture lost its original album", albums)
	}
	albumID := albums.Items[0].ID
	indexedAt := albums.Items[0].IndexedAt
	var items struct {
		Items []media.Metadata `json:"items"`
	}
	api.request("GET", "/albums/"+albumID+"/media", nil, 200, &items)
	root := filepath.Dir(marker)
	expected := make(map[string][32]byte)
	for _, item := range items.Items {
		data, err := os.ReadFile(filepath.Join(root, "TaggedAlbum", item.FileName))
		if err != nil {
			t.Fatal(err)
		}
		expected[item.FileName] = sha256.Sum256(data)
	}
	copyBytes, err := os.ReadFile(filepath.Join(root, "TaggedAlbum", "photo.png"))
	if err != nil {
		t.Fatal(err)
	}
	// Add only a test-owned external read-only source file to obtain a second
	// native mapped creator; no production seed, mutation hook or source writer.
	if err := os.Mkdir(filepath.Join(root, "ManualCreator"), 0o700); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(root, "ManualCreator", "portrait.png")
	if err := os.WriteFile(otherPath, copyBytes, 0o400); err != nil {
		t.Fatal(err)
	}
	scan := func() {
		t.Helper()
		var job jobs.Job
		api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &job)
		if result := api.waitJob(job.ID); result.Status != "succeeded" || result.Done != workUnits+6 || result.Failed != 0 {
			t.Fatal("creator native scan failed", result, result.Counts)
		}
	}
	scan()
	assertCompiledAlbumOrder(t, api, source.ID, 2)
	var people struct {
		Items []access.Person `json:"items"`
	}
	api.request("GET", "/people", nil, 200, &people)
	var selected string
	for _, person := range people.Items {
		if person.SourceID == source.ID && person.Name == "ManualCreator" {
			selected = person.ID
		}
	}
	if selected == "" {
		t.Fatal("native second creator was not indexed", people)
	}
	path := "/admin/albums/" + albumID + "/creator"
	var before access.AlbumCreator
	initial := api.request("GET", path, nil, 200, &before)
	if before.Mode != "mapped" || before.MappedPersonID == nil || before.PersonID == nil || *before.PersonID != *before.MappedPersonID {
		t.Fatal("native creator provenance was not explicit", before)
	}
	mapped := *before.MappedPersonID
	body := map[string]string{"mode": "person", "person_id": selected}
	headers := http.Header{"If-Match": {initial.Get("ETag")}, "Idempotency-Key": {store.NewID()}}
	var changed access.AlbumCreator
	changedHeaders := api.request("PUT", path, body, 200, &changed, headers)
	if changed.Mode != "person" || changed.PersonID == nil || *changed.PersonID != selected || changed.MappedPersonID == nil || *changed.MappedPersonID != mapped || changed.Version != before.Version+1 {
		t.Fatal("compiled manual creator override lost fallback", changed, before)
	}
	scan()
	var stable access.AlbumCreator
	api.request("GET", path, nil, 200, &stable)
	if !reflect.DeepEqual(stable, changed) {
		t.Fatal("no-op scan overwrote creator choice/provenance", stable, changed)
	}
	if err := os.Rename(filepath.Join(root, "TaggedAlbum"), filepath.Join(root, "CreatorMoved")); err != nil {
		t.Fatal(err)
	}
	scan()
	latest := api.request("GET", path, nil, 200, &stable)
	if stable.Mode != "person" || stable.PersonID == nil || *stable.PersonID != selected || stable.MappedPersonID == nil || *stable.MappedPersonID != mapped || stable.OverridePersonID == nil || *stable.OverridePersonID != selected || stable.Version <= changed.Version {
		t.Fatal("folder move confused override with mapped creator identity", stable, changed)
	}
	var selectedPerson access.Person
	api.request("GET", "/people/"+selected, nil, 200, &selectedPerson)
	if selectedPerson.Name != "ManualCreator" || selectedPerson.AlbumCount != 2 {
		t.Fatal("mapped folder move stole the selected creator identity", selectedPerson)
	}
	var historical access.AlbumCreator
	replayed := api.request("PUT", path, body, 200, &historical, headers)
	if !reflect.DeepEqual(historical, changed) || replayed.Get("ETag") != changedHeaders.Get("ETag") {
		t.Fatal("creator scan/move retargeted historical receipt", historical, changed)
	}
	var none access.AlbumCreator
	api.request("PUT", path, map[string]string{"mode": "none"}, 200, &none, http.Header{"If-Match": {latest.Get("ETag")}})
	if none.Mode != "none" || none.PersonID != nil || none.MappedPersonID == nil || *none.MappedPersonID != mapped {
		t.Fatal("explicit none mode discarded indexed fallback", none)
	}
	if err := os.Rename(filepath.Join(root, "CreatorMoved"), filepath.Join(root, "CreatorFinal")); err != nil {
		t.Fatal(err)
	}
	scan()
	latest = api.request("GET", path, nil, 200, &stable)
	if stable.Mode != "none" || stable.PersonID != nil || stable.MappedPersonID == nil || *stable.MappedPersonID != mapped {
		t.Fatal("scan silently restored an explicitly absent creator", stable)
	}
	var restored access.AlbumCreator
	api.request("PUT", path, map[string]string{"mode": "mapped"}, 200, &restored, http.Header{"If-Match": {latest.Get("ETag")}})
	if restored.Mode != "mapped" || restored.PersonID == nil || *restored.PersonID != mapped || restored.OverridePersonID != nil {
		t.Fatal("mapped restoration lost current source provenance", restored)
	}
	var mappedPerson access.Person
	api.request("GET", "/people/"+mapped, nil, 200, &mappedPerson)
	if mappedPerson.Name != "CreatorFinal" {
		t.Fatal("restoration used an old or manually selected folder name", mappedPerson)
	}
	var current struct {
		Items []media.Metadata `json:"items"`
	}
	api.request("GET", "/albums/"+albumID+"/media", nil, 200, &current)
	if len(current.Items) != len(items.Items) {
		t.Fatal("creator classification changed original album media count", current)
	}
	for index, item := range items.Items {
		path := filepath.Join(root, "CreatorFinal", item.FileName)
		info, err := os.Stat(path)
		data, readErr := os.ReadFile(path)
		if err != nil || readErr != nil || info.Mode().Perm() != 0o400 || sha256.Sum256(data) != expected[item.FileName] || current.Items[index].ID != item.ID {
			t.Fatal("creator API/scan/move changed source bytes/modes/media identity", item.ID, err, readErr)
		}
	}
	info, err := os.Stat(otherPath)
	data, readErr := os.ReadFile(otherPath)
	if err != nil || readErr != nil || info.Mode().Perm() != 0o400 || sha256.Sum256(data) != sha256.Sum256(copyBytes) {
		t.Fatal("creator classification changed the additional read-only source", err, readErr)
	}
	var orderedAlbum access.Album
	api.request("GET", "/albums/"+albumID, nil, 200, &orderedAlbum)
	if !orderedAlbum.IndexedAt.Equal(indexedAt) {
		t.Fatal("native creator choices/no-op/folder moves changed first-index time", orderedAlbum, indexedAt)
	}
	assertCompiledAlbumOrder(t, api, source.ID, 2)
	anonymous := *api.client
	anonymous.Jar = nil
	(&scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}).request("GET", path, nil, 401, nil)
}
