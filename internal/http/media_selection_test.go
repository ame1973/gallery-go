package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/media"
)

func TestAlbumMediaHTTPSelectionBindsSortCaptureAccountScopeAndCollection(t *testing.T) {
	f := protectedMedia(t)
	captured := time.Date(2026, 11, 6, 1, 2, 3, 123000000, time.UTC)
	for _, row := range []struct{ id, kind string }{{"page-a", "image"}, {"page-b", "video"}, {"page-c", "image"}} {
		instant := captured
		if row.id == "page-c" {
			instant = instant.Add(time.Millisecond)
		}
		if _, err := f.db.Exec(`INSERT INTO media(id,album_id,kind,file_name,capture_at,created_at,updated_at) VALUES(?,'media-album',?,'Processing.png',?,0,0)`, row.id, row.kind, instant.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	const path = "/api/v2/albums/media-album/media"
	response := f.reader.request("GET", path+"?sort=captured_desc&limit=1", nil, "", "")
	first := decodeResponse[listPage[media.Metadata]](t, response, 200)
	if len(first.Items) != 1 || first.Items[0].ID != "page-c" || !first.HasMore || first.NextCursor == nil || len(*first.NextCursor) > 512 || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("media selection is not bounded/private/capture ordered", first, response.Header())
	}
	var cursor pageCursor
	decoded, err := base64.RawURLEncoding.DecodeString(*first.NextCursor)
	if err != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.After != "page-c" || cursor.Position == "" {
		t.Fatal("capture key was not frozen in the bounded cursor", string(decoded), err)
	}
	second := decodeResponse[listPage[media.Metadata]](t, f.reader.request("GET", path+"?sort=captured_desc&limit=1&cursor="+url.QueryEscape(*first.NextCursor), nil, "", ""), 200)
	if len(second.Items) != 1 || second.Items[0].ID != "page-b" || second.NextCursor == nil {
		t.Fatal("capture keyset lost the next timestamp/tie", second)
	}
	end := decodeResponse[listPage[media.Metadata]](t, f.reader.request("GET", path+"?sort=captured_desc&limit=100&cursor="+url.QueryEscape(*second.NextCursor), nil, "", ""), 200)
	if len(end.Items) != 2 || end.Items[0].ID != "page-a" || end.Items[1].ID != "protected" || end.HasMore || end.NextCursor != nil || end.Items[1].CaptureAt != nil {
		t.Fatal("capture tie/null ordering or changed page size is inconsistent", end)
	}
	for _, sort := range []string{"id_asc", "id_desc", "captured_asc", "captured_desc"} {
		query := url.Values{"sort": {sort}, "kind": {"image"}, "limit": {"1"}}
		ids := []string{}
		for range 4 {
			page := decodeResponse[listPage[media.Metadata]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
			for _, item := range page.Items {
				ids = append(ids, item.ID)
				if item.Kind != "image" {
					t.Fatal("kind filter ran after pagination", item)
				}
			}
			if page.NextCursor == nil {
				break
			}
			query.Set("cursor", *page.NextCursor)
		}
		want := []string{"page-a", "page-c", "protected"}
		switch sort {
		case "id_desc":
			slices.Reverse(want)
		case "captured_asc":
			want = []string{"protected", "page-a", "page-c"}
		case "captured_desc":
			want = []string{"page-c", "page-a", "protected"}
		}
		if !slices.Equal(ids, want) {
			t.Fatal("HTTP pages lost or repeated a sort/null key", sort, ids, want)
		}
	}
	bounds := url.Values{"captured_from": {captured.Format(time.RFC3339Nano)}, "captured_before": {captured.Add(time.Millisecond).Format(time.RFC3339Nano)}, "sort": {"captured_desc"}}
	selected := decodeResponse[listPage[media.Metadata]](t, f.reader.request("GET", path+"?"+bounds.Encode(), nil, "", ""), 200)
	if len(selected.Items) != 2 || selected.Items[0].ID != "page-b" || selected.Items[1].ID != "page-a" {
		t.Fatal("capture interval was not inclusive/exclusive before limit", selected)
	}
	bounds.Set("captured_from", captured.In(time.FixedZone("explicit", 8*3600)).Format(time.RFC3339Nano))
	withOffset := decodeResponse[listPage[media.Metadata]](t, f.reader.request("GET", path+"?"+bounds.Encode(), nil, "", ""), 200)
	if !slices.EqualFunc(selected.Items, withOffset.Items, func(a, b media.Metadata) bool { return a.ID == b.ID }) {
		t.Fatal("explicit offset used host timezone instead of the instant", withOffset)
	}
	for _, query := range []string{"", "sort=id_desc&", "sort=captured_asc&", "sort=captured_desc&kind=image&"} {
		expectCode(t, f.reader.request("GET", path+"?"+query+"cursor="+url.QueryEscape(*first.NextCursor), nil, "", ""), 400, "INVALID_CURSOR")
	}
	expectCode(t, f.admin.request("GET", path+"?sort=captured_desc&cursor="+url.QueryEscape(*first.NextCursor), nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, f.reader.request("GET", "/api/v2/media/protected/variants?sort=captured_desc&cursor="+url.QueryEscape(*first.NextCursor), nil, "", ""), 400, "INVALID_CURSOR")
	for _, value := range []string{"", "invalid", "+1", "01", "9223372036854775807", strings.Repeat("1", 33)} {
		changed := cursor
		changed.Position = value
		data, _ := json.Marshal(changed)
		expectCode(t, f.reader.request("GET", path+"?sort=captured_desc&cursor="+base64.RawURLEncoding.EncodeToString(data), nil, "", ""), 400, "INVALID_CURSOR")
	}
	filtered := decodeResponse[listPage[media.Metadata]](t, f.reader.request("GET", path+"?sort=captured_desc&kind=image&limit=1", nil, "", ""), 200)
	reordered := decodeResponse[listPage[media.Metadata]](t, f.reader.request("GET", path+"?cursor="+url.QueryEscape(*filtered.NextCursor)+"&kind=image&sort=captured_desc&limit=1", nil, "", ""), 200)
	if len(reordered.Items) != 1 || reordered.Items[0].ID != "page-a" {
		t.Fatal("query key ordering invalidated a canonical cursor", reordered)
	}
	if _, err := f.db.Exec(`UPDATE users SET scope_id='rotated-media-selection',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?sort=captured_desc&cursor="+url.QueryEscape(*first.NextCursor), nil, "", ""), 400, "INVALID_CURSOR")
	if _, err := f.db.Exec(`DELETE FROM library_access`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?kind=video", nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.reader.request("GET", "/api/v2/albums/missing/media?kind=video", nil, "", ""), 404, "NOT_FOUND")
	for _, secret := range []string{f.source, "source_sha256", "root_path", "relative_path", "storage_path", "GPS"} {
		if strings.Contains(response.Body.String()+string(decoded), secret) {
			t.Fatal("media position/selection exposed private implementation", secret)
		}
	}
}

func TestAlbumMediaHTTPRejectsUnknownAmbiguousAndNonInstantSelections(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/albums/media-album/media"
	for _, query := range []string{
		"kind=", "kind=image&kind=video", "kind=Image", "kind=animated", "q=private", "source_id=private",
		"sort=", "sort=latest", "sort=id_asc&sort=id_desc", "sort=" + url.QueryEscape("id_desc; DROP TABLE media"),
		"captured_from=", "captured_before=bad", "captured_from=2026-11-06", "captured_from=2026-11-06T00:00:00",
		"captured_from=2026-11-06T00:00:00-00:00", "captured_from=2026-11-06T00:00:00.000001Z",
		"captured_from=2026-11-06T00:00:00,123Z", "captured_from=2026-11-06T1:00:00Z",
		"captured_from=" + url.QueryEscape("2026-11-06T00:00:00+24:00"),
		"captured_from=" + url.QueryEscape("2026-11-06T00:00:00+00:60"),
		"captured_from=0000-01-01T00:00:00Z", "captured_before=2026-02-30T00:00:00Z",
		"captured_from=2026-11-06T00:00:00Z&captured_before=2026-11-06T00:00:00Z",
		"captured_from=2026-11-07T00:00:00Z&captured_before=2026-11-06T00:00:00Z",
		"captured_from=2026-11-06T00:00:00Z&captured_from=2026-11-07T00:00:00Z",
	} {
		expectCode(t, f.reader.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_FILTER")
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "cursor=x&cursor=y", "limit=%zz"} {
		expectCode(t, f.reader.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	expectCode(t, f.reader.request("GET", path+"?cursor=", nil, "", ""), 400, "INVALID_CURSOR")
	anonymous := adminClient{handler: f.http}
	expectCode(t, anonymous.request("GET", path+"?sort=wrong", nil, "", ""), 401, "UNAUTHENTICATED")
}
