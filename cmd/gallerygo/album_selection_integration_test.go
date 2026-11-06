package main

import (
	"net/url"
	"reflect"
	"slices"
	"testing"

	"gallerygo/internal/access"
)

// Called from all four actual native scan modes, using IDs produced by indexed
// mapping/tag APIs rather than synthetic albums, labels or source paths.
func assertCompiledAlbumSelection(t *testing.T, api *scanHTTPSClient, filter access.AlbumFilter, wantIDs ...string) {
	t.Helper()
	query := url.Values{"limit": {"100"}}
	for key, value := range map[string]string{"library_id": filter.LibraryID, "source_id": filter.SourceID, "person_id": filter.PersonID, "tag_id": filter.TagID} {
		if value != "" {
			query.Set(key, value)
		}
	}
	var page struct {
		Items      []access.Album `json:"items"`
		NextCursor *string        `json:"next_cursor"`
		HasMore    bool           `json:"has_more"`
	}
	headers := api.request("GET", "/albums?"+query.Encode(), nil, 200, &page)
	ids := []string{}
	for _, album := range page.Items {
		ids = append(ids, album.ID)
		var detail access.Album
		api.request("GET", "/albums/"+album.ID, nil, 200, &detail)
		if !reflect.DeepEqual(album, detail) {
			t.Fatal("native selection changed the shared authorized album projection", album, detail)
		}
	}
	if page.Items == nil || !slices.Equal(ids, wantIDs) || page.NextCursor != nil || page.HasMore || headers.Get("Cache-Control") != "private, no-store" {
		t.Fatal("native selection lost indexed IDs, bounded pages or privacy", query, page, wantIDs, headers)
	}
}
