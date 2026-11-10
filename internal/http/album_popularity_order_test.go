package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func albumPopularityHTTPFixture(t *testing.T) (protectedFixture, auth.User) {
	t.Helper()
	f := albumOrderHTTPFixture(t)
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at)
 VALUES('selection-zero-album','media-library','media-source','selection-person','Never viewed',0,0);
 INSERT INTO album_tags VALUES('selection-zero-album','selection-tag')`); err != nil {
		t.Fatal(err)
	}
	third := createHTTPUser(t, f.admin, "popular-third-viewer", "user")
	decodeResponse[access.UserGroups](t, f.admin.request("PUT", "/api/v2/admin/users/"+third.ID+"/groups", map[string]any{"group_ids": []string{"media-group"}}, userETag(third), ""), 200)
	thirdClient := accountClient(t, f.http, third.Username, testPassword)
	for _, item := range []struct {
		client adminClient
		id     string
		count  int
	}{
		{f.admin, "selection-tie-album", 2}, {thirdClient, "selection-tie-album", 1},
		{f.admin, "media-album", 2}, {thirdClient, "media-album", 1},
		{f.admin, "selection-other-album", 2}, {f.admin, "selection-private-album", 4},
	} {
		for range item.count {
			path := "/api/v2/albums/" + item.id + "/views"
			decodeResponse[access.AlbumView](t, item.client.request("POST", path, nil, "", store.NewID()), 200)
		}
	}
	return f, third
}

func TestAlbumPopularityOrderHTTPFreezesPublicTotalsAndBindsScope(t *testing.T) {
	f, _ := albumPopularityHTTPFixture(t)
	const path = "/api/v2/albums"
	query := url.Values{"library_id": {"media-library"}, "source_id": {"media-source"}, "person_id": {"selection-person"}, "tag_id": {"selection-tag"}, "sort": {"popular"}, "limit": {"1"}}
	want := []string{"selection-tie-album", "media-album", "selection-other-album", "selection-zero-album"}
	seen := []string{}
	firstCursor := ""
	var first pageCursor
	for range 5 {
		response := f.reader.request("GET", path+"?"+query.Encode(), nil, "", "")
		page := decodeResponse[listPage[access.Album]](t, response, 200)
		if len(page.Items) != 1 || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("popular page was not bounded, authorized and private", page, response.Header())
		}
		album := page.Items[0]
		seen = append(seen, album.ID)
		detail := decodeResponse[access.Album](t, f.reader.request("GET", path+"/"+album.ID, nil, "", ""), 200)
		if !reflect.DeepEqual(album, detail) {
			t.Fatal("popular card differs from the common authorized detail", album, detail)
		}
		if page.NextCursor == nil {
			if page.HasMore {
				t.Fatal("terminal popularity page lost its continuation")
			}
			break
		}
		data, err := base64.RawURLEncoding.DecodeString(*page.NextCursor)
		var cursor pageCursor
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Position != access.AlbumPopularityKey(album.ViewCount) || cursor.After != album.ID {
			t.Fatal("popularity cursor did not freeze the complete public count", cursor, album, err)
		}
		for _, secret := range []string{f.source, "root_path", "relative_path", "source_sha256", "storage_path", "user_id"} {
			if strings.Contains(response.Body.String()+string(data), secret) {
				t.Fatal("popularity page leaked a private source/account key", secret)
			}
		}
		if firstCursor == "" {
			firstCursor, first = *page.NextCursor, cursor
		}
		query.Set("cursor", *page.NextCursor)
	}
	if !slices.Equal(seen, want) {
		t.Fatal("popular pages lost totals, ties, zeros or pre-limit visibility", seen, want)
	}
	query.Set("cursor", firstCursor)
	expectCode(t, f.admin.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("sort", "top_rated")
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("sort", "popular")
	for _, key := range []string{"", "3", "r:3", "n:Mw", "v:", "v:-1", "v:+1", "v:01", "v:1.0", "v:1e0", "v:9223372036854775808", "v:" + strings.Repeat("1", 33)} {
		invalid := first
		invalid.Position = key
		data, _ := json.Marshal(invalid)
		query.Set("cursor", base64.RawURLEncoding.EncodeToString(data))
		expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	}
	query.Del("cursor")
	if _, err := f.db.Exec(`DROP INDEX idx_album_person_view_page`); err != nil {
		t.Fatal(err)
	}
	failed := f.reader.request("GET", path+"?"+query.Encode(), nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	for _, secret := range []string{"idx_album", "SQL", f.source} {
		if strings.Contains(problemContent(t, failed), secret) {
			t.Fatal("popular query failure exposed private database/source details", failed.Body.String())
		}
	}
}

func TestAlbumPopularityOrderHTTPViewDeleteAndMissingAnchorPreserveFrozenKey(t *testing.T) {
	f, third := albumPopularityHTTPFixture(t)
	const path = "/api/v2/albums"
	query := url.Values{"source_id": {"media-source"}, "tag_id": {"selection-tag"}, "sort": {"popular"}, "limit": {"1"}}
	first := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if first.NextCursor == nil || len(first.Items) != 1 || first.Items[0].ID != "selection-tie-album" || first.Items[0].ViewCount != 3 {
		t.Fatal(first)
	}
	viewPath := "/api/v2/albums/selection-tie-album/views"
	key := store.NewID()
	one := decodeResponse[access.AlbumView](t, f.admin.request("POST", viewPath, nil, "", key), 200)
	if replay := decodeResponse[access.AlbumView](t, f.admin.request("POST", viewPath, nil, "", key), 200); !reflect.DeepEqual(replay, one) {
		t.Fatal("real view receipt did not replay its historical state", one, replay)
	}
	query.Set("cursor", *first.NextCursor)
	next := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if len(next.Items) != 1 || next.Items[0].ID != "media-album" || next.Items[0].ViewCount != 3 {
		t.Fatal("popular continuation reread the anchor's higher current total", next)
	}
	userPath := "/api/v2/admin/users/" + third.ID
	user := f.admin.request("GET", userPath, nil, "", "")
	if response := f.admin.request("DELETE", userPath, nil, user.Header().Get("ETag"), ""); response.Code != 204 {
		t.Fatal("real account deletion failed", response.Code, response.Body.String())
	}
	query.Del("cursor")
	query.Set("limit", "100")
	current := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	want := []string{"selection-tie-album", "selection-other-album", "media-album", "selection-zero-album"}
	ids := []string{}
	for _, item := range current.Items {
		ids = append(ids, item.ID)
	}
	if !slices.Equal(ids, want) || current.Items[0].ViewCount != 3 || current.Items[1].ViewCount != 2 || current.Items[2].ViewCount != 2 {
		t.Fatal("account deletion did not update the indexed public count", current, want)
	}
	if _, err := f.db.Exec(`DELETE FROM albums WHERE id='selection-tie-album'`); err != nil {
		t.Fatal(err)
	}
	query.Set("cursor", *first.NextCursor)
	query.Set("limit", "1")
	next = decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if len(next.Items) != 1 || next.Items[0].ID != "selection-other-album" {
		t.Fatal("popular continuation required its deleted anchor", next)
	}
	if _, err := f.db.Exec(`UPDATE users SET scope_id='new-popular-order-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
}
