package main

import (
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/media"
)

// Compare the compiled HTTPS selector against the small, genuinely inspected
// fixture corpus. This is not a production Go-side full-result sort or seed route.
func assertCompiledMediaSelection(t *testing.T, api *scanHTTPSClient, album string, corpus []media.Metadata) {
	t.Helper()
	for _, kind := range []string{"", "image", "video"} {
		for _, order := range []string{"id_asc", "id_desc", "captured_asc", "captured_desc"} {
			expected := []media.Metadata{}
			for _, item := range corpus {
				if kind == "" || item.Kind == kind {
					expected = append(expected, item)
				}
			}
			slices.SortFunc(expected, func(a, b media.Metadata) int {
				comparison := 0
				if strings.HasPrefix(order, "captured_") {
					switch {
					case a.CaptureAt == nil && b.CaptureAt != nil:
						comparison = -1
					case a.CaptureAt != nil && b.CaptureAt == nil:
						comparison = 1
					case a.CaptureAt != nil && b.CaptureAt != nil:
						comparison = a.CaptureAt.Compare(*b.CaptureAt)
					}
				}
				if comparison == 0 {
					comparison = strings.Compare(a.ID, b.ID)
				}
				if strings.HasSuffix(order, "_desc") {
					comparison = -comparison
				}
				return comparison
			})
			query := url.Values{"sort": {order}, "limit": {"3"}}
			if kind != "" {
				query.Set("kind", kind)
			}
			actual := []media.Metadata{}
			for range len(corpus) + 1 {
				var page struct {
					Items      []media.Metadata `json:"items"`
					NextCursor *string          `json:"next_cursor"`
					HasMore    bool             `json:"has_more"`
				}
				api.request("GET", "/albums/"+album+"/media?"+query.Encode(), nil, 200, &page)
				if len(page.Items) > 3 || page.HasMore != (page.NextCursor != nil) {
					t.Fatal("compiled selector invented a page boundary", page)
				}
				actual = append(actual, page.Items...)
				if page.NextCursor == nil {
					break
				}
				query.Set("cursor", *page.NextCursor)
			}
			if !slices.EqualFunc(expected, actual, func(a, b media.Metadata) bool {
				return a.ID == b.ID && a.Revision != nil && b.Revision != nil && *a.Revision == *b.Revision
			}) {
				t.Fatal("compiled native selection lost/duplicated or retargeted media/revisions", kind, order, expected, actual)
			}
		}
	}
	from := time.Date(2026, 11, 5, 4, 34, 56, 123000000, time.UTC)
	before := from.Add(time.Millisecond)
	query := url.Values{"kind": {"image"}, "captured_from": {from.Format(time.RFC3339Nano)}, "captured_before": {before.Format(time.RFC3339Nano)}, "sort": {"captured_desc"}}
	var selected struct {
		Items []media.Metadata `json:"items"`
	}
	api.request("GET", "/albums/"+album+"/media?"+query.Encode(), nil, 200, &selected)
	expected := 0
	for _, item := range corpus {
		if item.Kind == "image" && item.CaptureAt != nil && !item.CaptureAt.Before(from) && item.CaptureAt.Before(before) {
			expected++
		}
	}
	if expected == 0 || len(selected.Items) != expected {
		t.Fatal("compiled source-proven capture range counted unknown/mtime/video instants", expected, selected)
	}
}
