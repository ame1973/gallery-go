package main

import (
	"cmp"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
)

type compiledAlbumPage struct {
	Items      []access.Album `json:"items"`
	NextCursor *string        `json:"next_cursor"`
	HasMore    bool           `json:"has_more"`
}

// Called from all four actual native scan modes, using IDs produced by indexed
// mapping/tag APIs rather than synthetic albums, labels or source paths.
func assertCompiledAlbumSelection(t *testing.T, api *scanHTTPSClient, filter access.AlbumFilter, wantIDs ...string) {
	t.Helper()
	query := url.Values{"limit": {"100"}}
	for key, value := range map[string]string{"library_id": filter.LibraryID, "source_id": filter.SourceID, "person_id": filter.PersonID, "tag_id": filter.TagID, "favorite": filter.Favorite, "sort": filter.Sort} {
		if value != "" {
			query.Set(key, value)
		}
	}
	var page compiledAlbumPage
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

func assertCompiledAlbumOrder(t *testing.T, api *scanHTTPSClient, sourceID string, wantCount int) {
	t.Helper()
	query := url.Values{"source_id": {sourceID}, "limit": {"100"}}
	var corpus compiledAlbumPage
	api.request("GET", "/albums?"+query.Encode(), nil, 200, &corpus)
	if len(corpus.Items) != wantCount || corpus.HasMore || corpus.NextCursor != nil {
		t.Fatal("native ordered fixture lost actual published albums", corpus, wantCount)
	}
	// Sorting these at-most-two expected fixture cards is not a production catalog
	// sort. The real socket endpoint must independently page the SQL order.
	for _, order := range []string{"oldest", "latest", "name_asc", "name_desc", "top_rated"} {
		expected := slices.Clone(corpus.Items)
		slices.SortFunc(expected, func(a, b access.Album) int {
			if order == "top_rated" {
				one, two := 0.0, 0.0
				if a.RatingAvg != nil {
					one = *a.RatingAvg
				}
				if b.RatingAvg != nil {
					two = *b.RatingAvg
				}
				if compared := cmp.Compare(one, two); compared != 0 {
					return compared
				}
			} else if strings.HasPrefix(order, "name_") {
				if compared := strings.Compare(a.Title, b.Title); compared != 0 {
					return compared
				}
			} else if compared := a.IndexedAt.Compare(b.IndexedAt); compared != 0 {
				return compared
			}
			return strings.Compare(a.ID, b.ID)
		})
		if order == "latest" || order == "name_desc" || order == "top_rated" {
			slices.Reverse(expected)
		}
		query.Set("sort", order)
		query.Set("limit", "1")
		query.Del("cursor")
		for index, want := range expected {
			var page compiledAlbumPage
			headers := api.request("GET", "/albums?"+query.Encode(), nil, 200, &page)
			if len(page.Items) != 1 || !reflect.DeepEqual(page.Items[0], want) || headers.Get("Cache-Control") != "private, no-store" {
				t.Fatal("actual native ordered page changed tuple order or public projection", order, page, want)
			}
			if index == len(expected)-1 {
				if page.HasMore || page.NextCursor != nil {
					t.Fatal("native ordered terminal page has an extra continuation", page)
				}
			} else {
				if !page.HasMore || page.NextCursor == nil {
					t.Fatal("native ordered page lost its frozen continuation", page)
				}
				query.Set("cursor", *page.NextCursor)
			}
		}
	}
}
