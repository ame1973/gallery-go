package main

import (
	"crypto/sha256"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func verifyCompiledTagCatalog(t *testing.T, api *scanHTTPSClient) {
	t.Helper()
	var me auth.User
	api.request("GET", "/auth/me", nil, 200, &me)
	createKey := store.NewID()
	var created access.Tag
	initial := api.request("POST", "/admin/tags", map[string]string{"name": " CLI catalog tag "}, 201, &created, http.Header{"Idempotency-Key": {createKey}})
	if created.Name != "CLI catalog tag" || created.Version != 1 || initial.Get("Location") != "/api/v2/admin/tags/"+created.ID || initial.Get("ETag") == "" {
		t.Fatal("compiled tag creation did not expose exact versioned metadata", created, initial)
	}
	path := "/admin/tags/" + created.ID
	renameHeaders := http.Header{"If-Match": {initial.Get("ETag")}, "Idempotency-Key": {store.NewID()}}
	var changed access.Tag
	current := api.request("PATCH", path, map[string]string{"name": "CLI renamed tag"}, 200, &changed, renameHeaders)
	if changed.ID != created.ID || changed.Name != "CLI renamed tag" || changed.Version != 2 {
		t.Fatal("compiled rename lost tag identity/version", created, changed)
	}
	var noop access.Tag
	api.request("PATCH", path, map[string]string{"name": " CLI renamed tag "}, 200, &noop, http.Header{"If-Match": {current.Get("ETag")}})
	if noop != changed {
		t.Fatal("compiled same-name command was not a version no-op", noop, changed)
	}
	api.request("POST", "/admin/tags", map[string]string{"name": "CLI renamed tag"}, 409, nil)
	var historical access.Tag
	replayed := api.request("POST", "/admin/tags", map[string]string{"name": "CLI catalog tag"}, 201, &historical, http.Header{"Idempotency-Key": {createKey}})
	if historical != created || replayed.Get("ETag") != initial.Get("ETag") {
		t.Fatal("compiled creation replay was retargeted by rename", historical, created)
	}
	api.request("GET", path, nil, 200, &historical)
	if historical != changed {
		t.Fatal("current tag GET did not distinguish historical receipts", historical, changed)
	}
	api.request("DELETE", path, nil, 409, nil, http.Header{"If-Match": {initial.Get("ETag")}})
	deleteHeaders := http.Header{"If-Match": {current.Get("ETag")}, "Idempotency-Key": {store.NewID()}}
	for range 2 {
		api.request("DELETE", path, nil, 204, nil, deleteHeaders)
	}
	api.request("GET", path, nil, 404, nil)
	api.request("PATCH", path, map[string]string{"name": "CLI renamed tag"}, 200, &historical, renameHeaders)
	if historical != changed {
		t.Fatal("compiled rename receipt lost history after deletion", historical, changed)
	}
	var after auth.User
	api.request("GET", "/auth/me", nil, 200, &after)
	if after.ScopeID != me.ScopeID {
		t.Fatal("tag catalog commands changed authorization scope", me.ScopeID, after.ScopeID)
	}
	anonymous := *api.client
	anonymous.Jar = nil
	guest := &scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}
	guest.request("GET", "/admin/tags", nil, 401, nil)
	guest.request("GET", path, nil, 401, nil)
}

func verifyCompiledAlbumTagAssociations(t *testing.T, api *scanHTTPSClient, source library.Source, marker string, workUnits int64) {
	t.Helper()
	var albums struct {
		Items []access.Album `json:"items"`
	}
	api.request("GET", "/albums", nil, 200, &albums)
	if len(albums.Items) != 1 {
		t.Fatal("album-tag fixture lost its single logical album", albums)
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
		data, err := os.ReadFile(filepath.Join(root, "RenamedCreator", item.FileName))
		if err != nil {
			t.Fatal(err)
		}
		expected[item.FileName] = sha256.Sum256(data)
	}
	var one, two access.Tag
	api.request("POST", "/admin/tags", map[string]string{"name": "CLI assigned one"}, 201, &one)
	api.request("POST", "/admin/tags", map[string]string{"name": "CLI assigned two"}, 201, &two)
	ids := []string{one.ID, two.ID}
	slices.Sort(ids)
	path := "/admin/albums/" + albumID + "/tags"
	var before access.AlbumTagSet
	initial := api.request("GET", path, nil, 200, &before)
	body := map[string]any{"tag_ids": []string{two.ID, one.ID}}
	headers := http.Header{"If-Match": {initial.Get("ETag")}, "Idempotency-Key": {store.NewID()}}
	var changed access.AlbumTagSet
	changedHeaders := api.request("PUT", path, body, 200, &changed, headers)
	if changed.Version != before.Version+1 || !slices.Equal(changed.TagIDs, ids) {
		t.Fatal("compiled tag association did not publish its exact album version", changed, before)
	}
	var people struct {
		Items []access.Person `json:"items"`
	}
	api.request("GET", "/people", nil, 200, &people)
	if len(people.Items) != 1 || people.Items[0].SourceID != source.ID {
		t.Fatal("native album selection lost its source-bound mapped creator", people)
	}
	selection := access.AlbumFilter{LibraryID: source.LibraryID, SourceID: source.ID, PersonID: people.Items[0].ID, TagID: one.ID}
	for _, filter := range []access.AlbumFilter{
		{LibraryID: selection.LibraryID}, {SourceID: selection.SourceID},
		{PersonID: selection.PersonID}, {TagID: selection.TagID}, selection,
	} {
		assertCompiledAlbumSelection(t, api, filter, albumID)
	}
	for _, order := range []string{"latest", "oldest"} {
		chronological := selection
		chronological.Sort = order
		assertCompiledAlbumSelection(t, api, chronological, albumID)
	}
	assertCompiledAlbumSelection(t, api, access.AlbumFilter{SourceID: source.ID, PersonID: "unknown-creator"})
	scan := func() {
		t.Helper()
		var job jobs.Job
		api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &job)
		if result := api.waitJob(job.ID); result.Status != "succeeded" || result.Done != workUnits+3 || result.Failed != 0 {
			t.Fatal("native scan after association change failed", result, result.Counts)
		}
	}
	scan()
	var stable access.AlbumTagSet
	api.request("GET", path, nil, 200, &stable)
	if !reflect.DeepEqual(stable, changed) {
		t.Fatal("no-op scan changed tag associations or album version", stable, changed)
	}
	assertCompiledAlbumSelection(t, api, selection, albumID)
	// Only this test's temporary source folder moves externally. The application
	// must retain logical IDs and tag links without changing any 0400 file bytes.
	if err := os.Rename(filepath.Join(root, "RenamedCreator"), filepath.Join(root, "TaggedAlbum")); err != nil {
		t.Fatal(err)
	}
	scan()
	latest := api.request("GET", path, nil, 200, &stable)
	if stable.AlbumID != albumID || stable.Version <= changed.Version || !slices.Equal(stable.TagIDs, ids) {
		t.Fatal("native folder move lost tag links or logical album identity", stable, changed)
	}
	assertCompiledAlbumSelection(t, api, selection, albumID)
	var page struct {
		Items      []access.Tag `json:"items"`
		NextCursor *string      `json:"next_cursor"`
		HasMore    bool         `json:"has_more"`
	}
	publicPath := "/albums/" + albumID + "/tags"
	publicHeaders := api.request("GET", publicPath+"?limit=1", nil, 200, &page)
	if len(page.Items) != 1 || page.Items[0].ID != ids[0] || !page.HasMore || page.NextCursor == nil || publicHeaders.Get("ETag") != "" {
		t.Fatal("compiled public tags were not bounded or used an album-only label ETag", page, publicHeaders)
	}
	api.request("GET", publicPath+"?limit=1&cursor="+*page.NextCursor, nil, 200, &page)
	if len(page.Items) != 1 || page.Items[0].ID != ids[1] || page.HasMore || page.NextCursor != nil {
		t.Fatal("compiled album tag cursor lost a stable association", page)
	}
	var tag access.Tag
	tagHeaders := api.request("GET", "/admin/tags/"+one.ID, nil, 200, &tag)
	api.request("DELETE", "/admin/tags/"+one.ID, nil, 409, nil, http.Header{"If-Match": {tagHeaders.Get("ETag")}})
	api.request("PATCH", "/admin/tags/"+one.ID, map[string]string{"name": "Renamed assigned label"}, 200, &tag, http.Header{"If-Match": {tagHeaders.Get("ETag")}})
	var unchanged access.AlbumTagSet
	still := api.request("GET", path, nil, 200, &unchanged)
	if !reflect.DeepEqual(unchanged, stable) || still.Get("ETag") != latest.Get("ETag") {
		t.Fatal("tag label rename invalidated IDs-only association metadata", unchanged, stable)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	assertCompiledAlbumSelection(t, api, selection, albumID)
	assertCompiledAlbumOrder(t, api, source.ID, 1)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("album selection probed/recreated its missing source marker", err)
	}
	if err := os.WriteFile(marker, []byte("isolated scan marker"), 0o400); err != nil {
		t.Fatal(err)
	}
	var replay access.AlbumTagSet
	replayed := api.request("PUT", path, body, 200, &replay, headers)
	if !reflect.DeepEqual(replay, changed) || replayed.Get("ETag") != changedHeaders.Get("ETag") {
		t.Fatal("scan/move retargeted historical association receipts", replay, changed)
	}
	var cleared access.AlbumTagSet
	api.request("PUT", path, map[string]any{"tag_ids": []string{}}, 200, &cleared, http.Header{"If-Match": {latest.Get("ETag")}})
	if cleared.TagIDs == nil || len(cleared.TagIDs) != 0 || cleared.Version != stable.Version+1 {
		t.Fatal("explicit clear did not remove only the album links", cleared)
	}
	assertCompiledAlbumSelection(t, api, selection)
	for _, id := range ids {
		var value access.Tag
		current := api.request("GET", "/admin/tags/"+id, nil, 200, &value)
		api.request("DELETE", "/admin/tags/"+id, nil, 204, nil, http.Header{"If-Match": {current.Get("ETag")}})
	}
	var current struct {
		Items []media.Metadata `json:"items"`
	}
	api.request("GET", "/albums/"+albumID+"/media", nil, 200, &current)
	if len(current.Items) != len(items.Items) {
		t.Fatal("tag operations changed indexed media count", current)
	}
	for index, item := range items.Items {
		path := filepath.Join(root, "TaggedAlbum", item.FileName)
		info, err := os.Stat(path)
		data, readErr := os.ReadFile(path)
		if err != nil || readErr != nil || info.Mode().Perm() != 0o400 || sha256.Sum256(data) != expected[item.FileName] || current.Items[index].ID != item.ID {
			t.Fatal("tag association/scan/move changed source bytes/modes or media identity", item.ID, err, readErr)
		}
	}
	var orderedAlbum access.Album
	api.request("GET", "/albums/"+albumID, nil, 200, &orderedAlbum)
	if !orderedAlbum.IndexedAt.Equal(indexedAt) {
		t.Fatal("native tag/no-op/folder move changed the first-indexed ordering key", orderedAlbum, indexedAt)
	}
	anonymous := *api.client
	anonymous.Jar = nil
	guest := &scanHTTPSClient{t: t, client: &anonymous, origin: api.origin}
	guest.request("GET", publicPath, nil, 401, nil)
	guest.request("GET", path, nil, 401, nil)
}
