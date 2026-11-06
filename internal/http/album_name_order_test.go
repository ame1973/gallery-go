package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
)

func albumNameHTTPFixture(t *testing.T) (protectedFixture, string, string) {
	t.Helper()
	f := albumSelectionHTTPFixture(t)
	longID := strings.Repeat("&", 64)
	longTitle := strings.Repeat("\x01", access.MaxAlbumTitleBytes)
	if _, err := f.db.Exec(`UPDATE albums SET source_name='A' WHERE id='media-album';
 UPDATE albums SET source_name='z-source',display_name='A' WHERE id='selection-other-album';
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES('literal-empty','media-library','media-source','selection-person','',0,0);
 INSERT INTO album_tags VALUES('literal-empty','selection-tag')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES(?,'media-library','media-source','selection-person',?,0,0)`, longID, longTitle); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO album_tags(album_id,tag_id) VALUES(?,'selection-tag')`, longID); err != nil {
		t.Fatal(err)
	}
	return f, longID, longTitle
}

func TestAlbumNameOrderHTTPFullWorstCaseEnvelopeRoundTripsWithPrivateSharedCards(t *testing.T) {
	f, longID, longTitle := albumNameHTTPFixture(t)
	const path = "/api/v2/albums"
	for _, order := range []string{"name_asc", "name_desc"} {
		query := url.Values{"library_id": {"media-library"}, "source_id": {"media-source"}, "person_id": {"selection-person"}, "tag_id": {"selection-tag"}, "sort": {order}, "limit": {"1"}}
		want := []string{"literal-empty", longID, "media-album", "selection-other-album"}
		if order == "name_desc" {
			slices.Reverse(want)
		}
		seen := []string{}
		for range 5 {
			response := f.reader.request("GET", path+"?"+query.Encode(), nil, "", "")
			page := decodeResponse[listPage[access.Album]](t, response, 200)
			if len(page.Items) != 1 || response.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("literal name page is not bounded/private", page)
			}
			album := page.Items[0]
			seen = append(seen, album.ID)
			detail := decodeResponse[access.Album](t, f.reader.request("GET", path+"/"+url.PathEscape(album.ID), nil, "", ""), 200)
			if !reflect.DeepEqual(album, detail) || (album.ID == longID && album.Title != longTitle) || (album.ID == "media-album" && album.MediaCount != 1) {
				t.Fatal("name order changed shared title/count/cover semantics", album.ID)
			}
			if page.NextCursor == nil {
				if page.HasMore {
					t.Fatal("name terminal page lost its continuation", page)
				}
				break
			}
			if len(*page.NextCursor) > 8192 {
				t.Fatal("maximal title emitted an unfollowable cursor", len(*page.NextCursor))
			}
			if album.ID == longID && len(*page.NextCursor) <= 512 {
				t.Fatal("worst-case name cursor did not exercise its larger album-only bound")
			}
			data, err := base64.RawURLEncoding.DecodeString(*page.NextCursor)
			var cursor pageCursor
			if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.After != album.ID || cursor.Position != access.AlbumNameKey(album.Title) {
				t.Fatal("name cursor did not freeze the entire public key", album.ID, len(data), err)
			}
			query.Set("cursor", *page.NextCursor)
		}
		if !slices.Equal(seen, want) {
			t.Fatal("binary name pages lost empty/full/tied titles or widened grants", order, seen)
		}
	}
}

func TestAlbumNameOrderHTTPRejectsAmbiguousNameKeysAndRechecksScope(t *testing.T) {
	f, _, _ := albumNameHTTPFixture(t)
	const path = "/api/v2/albums"
	query := url.Values{"source_id": {"media-source"}, "tag_id": {"selection-tag"}, "sort": {"name_asc"}, "limit": {"1"}}
	first := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if first.NextCursor == nil {
		t.Fatal(first)
	}
	data, err := base64.RawURLEncoding.DecodeString(*first.NextCursor)
	var cursor pageCursor
	if err != nil || json.Unmarshal(data, &cursor) != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "0", "n:YQ==", "n:YR", "n:!", "n:_w", access.AlbumNameKey(strings.Repeat("a", access.MaxAlbumTitleBytes+1))} {
		invalid := cursor
		invalid.Position = key
		encoded, _ := json.Marshal(invalid)
		query.Set("cursor", base64.RawURLEncoding.EncodeToString(encoded))
		expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	}
	query.Set("cursor", strings.Repeat("x", 8193))
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("cursor", *first.NextCursor)
	expectCode(t, f.admin.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("sort", "name_desc")
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("sort", "name_asc")
	query.Set("tag_id", "other")
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("tag_id", "selection-tag")
	if _, err := f.db.Exec(`DELETE FROM albums WHERE id='literal-empty'`); err != nil {
		t.Fatal(err)
	}
	query.Set("limit", "100")
	continued := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if len(continued.Items) != 3 || continued.HasMore || continued.NextCursor != nil {
		t.Fatal("name continuation reread a deleted empty-title anchor", len(continued.Items))
	}
	if _, err := f.db.Exec(`UPDATE users SET scope_id='name-order-new-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
}

func TestAlbumNameOrderDoesNotExpandOrdinaryCursorBoundsOrEmitOversizeKeys(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	cursor := pageCursor{Scope: f.user.ScopeID, User: f.user.ID, Collection: "/api/v2/libraries", After: "a"}
	data, _ := json.Marshal(cursor)
	valid := base64.RawURLEncoding.EncodeToString(data)
	decodeResponse[listPage[access.Library]](t, f.reader.request("GET", "/api/v2/libraries?cursor="+valid, nil, "", ""), 200)
	cursor.Position = strings.Repeat("x", 33)
	data, _ = json.Marshal(cursor)
	expectCode(t, f.reader.request("GET", "/api/v2/libraries?cursor="+base64.RawURLEncoding.EncodeToString(data), nil, "", ""), 400, "INVALID_CURSOR")
	var padded map[string]any
	cursor.Position = ""
	data, _ = json.Marshal(cursor)
	if err := json.Unmarshal(data, &padded); err != nil {
		t.Fatal(err)
	}
	padded["ignored-padding"] = strings.Repeat("x", 600)
	data, _ = json.Marshal(padded)
	expectCode(t, f.reader.request("GET", "/api/v2/libraries?cursor="+base64.RawURLEncoding.EncodeToString(data), nil, "", ""), 400, "INVALID_CURSOR")
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-ID", "name-order-output-bound")
	page := pagination{limit: 1, cursorLimit: 512, cursor: pageCursor{Scope: strings.Repeat("s", 600)}}
	writePage(w, page, []string{"one", "two"}, func(id string) string { return id })
	expectCode(t, w, 500, "INTERNAL_ERROR")
	if strings.Contains(w.Body.String(), strings.Repeat("s", 50)) {
		t.Fatal("oversized continuation error leaked its internal scope")
	}
}

func TestAlbumNameOrderHTTPRejectsInvalidStoredTitlesWithoutInternalDisclosure(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	if _, err := f.db.Exec(`UPDATE albums SET source_name=? WHERE id='media-album'`, strings.Repeat("x", access.MaxAlbumTitleBytes+1)); err != nil {
		t.Fatal(err)
	}
	failed := f.reader.request("GET", "/api/v2/albums?source_id=media-source&sort=name_asc", nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	for _, secret := range []string{"source_name", "UTF-8", "SQL", f.source} {
		if strings.Contains(failed.Body.String(), secret) {
			t.Fatal("bad title exposed private internal evidence", failed.Body.String())
		}
	}
}
