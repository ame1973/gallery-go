package httpapi

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
)

func TestRandomAlbumsHTTPRespectsFreshVisibilityAndBoundedCards(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	const path = "/api/v2/home/random-albums"
	response := f.reader.request("GET", path, nil, "", "")
	var sample struct {
		Items []access.Album `json:"items"`
	}
	sample = decodeResponse[struct {
		Items []access.Album `json:"items"`
	}](t, response, 200)
	if sample.Items == nil || len(sample.Items) != 3 || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("random sample did not apply visibility before the bounded result", sample, response.Header())
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope) != 1 || envelope["items"] == nil {
		t.Fatal("sample exposed a cursor, count, or unexpected field", envelope, err)
	}
	seen := map[string]bool{}
	for _, album := range sample.Items {
		if seen[album.ID] {
			t.Fatal("sample repeated an album", album.ID)
		}
		seen[album.ID] = true
		detail := decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/"+album.ID, nil, "", ""), 200)
		if !reflect.DeepEqual(album, detail) {
			t.Fatal("sample card differs from authorized detail", album, detail)
		}
	}
	want := []string{"media-album", "selection-other-album", "selection-unmapped-album"}
	for _, id := range want {
		if !seen[id] {
			t.Fatal("sample omitted an eligible album", id, seen)
		}
	}
	for _, secret := range []string{f.source, "root_path", "relative_path", "source_sha256", "storage_path", "selection-private-album", "selection-foreign-album"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("random sample exposed a hidden resource or private source field", secret)
		}
	}
	for _, limit := range []string{"1", "2", "100"} {
		result := decodeResponse[struct {
			Items []access.Album `json:"items"`
		}](t, f.reader.request("GET", path+"?limit="+limit, nil, "", ""), 200)
		if result.Items == nil || len(result.Items) > 3 || limit == "1" && len(result.Items) != 1 || limit == "2" && len(result.Items) != 2 {
			t.Fatal("invalid sample size", limit, result)
		}
	}
	admin := decodeResponse[struct {
		Items []access.Album `json:"items"`
	}](t, f.admin.request("GET", path, nil, "", ""), 200)
	ids := make([]string, 0, len(admin.Items))
	for _, album := range admin.Items {
		ids = append(ids, album.ID)
	}
	if len(ids) != 5 || !slices.Contains(ids, "selection-private-album") || !slices.Contains(ids, "selection-foreign-album") {
		t.Fatal("administrator sample did not use its current authority", ids)
	}
	if _, err := f.db.Exec(`DELETE FROM library_access WHERE library_id='media-library' AND group_id='media-group'`); err != nil {
		t.Fatal(err)
	}
	empty := decodeResponse[struct {
		Items []access.Album `json:"items"`
	}](t, f.reader.request("GET", path, nil, "", ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 {
		t.Fatal("a removed library grant remained visible", empty)
	}
}

func TestRandomAlbumsHTTPRejectsInvalidQueriesAndAnonymousAccess(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	const path = "/api/v2/home/random-albums"
	for _, query := range []string{"limit=", "limit=0", "limit=101", "limit=-1", "limit=1&limit=2", "limit=1.5", "cursor=", "cursor=unused", "sort=latest", "unknown=1", "%ZZ=x"} {
		response := f.reader.request("GET", path+"?"+query, nil, "", "")
		if response.Code != 400 || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("invalid sample request was not rejected privately", query, response.Code, response.Body.String())
		}
	}
	anonymous := f.reader
	anonymous.cookie = nil
	expectCode(t, anonymous.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
}
