package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
)

func albumSelectionHTTPFixture(t *testing.T) protectedFixture {
	t.Helper()
	f := protectedMedia(t)
	if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('selection-foreign-library','Foreign',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('selection-foreign-source','selection-foreign-library','foreign','/never-open-album-http-source','{}','unavailable',0);
 INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('selection-person','media-source','Creator','Same label',0),('selection-foreign-person','selection-foreign-source','Other/Creator','Same label',0),('selection-no-path','media-source',NULL,'Not mapped',0);
 UPDATE albums SET person_id='selection-person',cover_media_id='protected' WHERE id='media-album';
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,access_restricted,created_at,updated_at) VALUES
 ('selection-other-album','media-library','media-source','selection-person','Other',0,0,0),
 ('selection-private-album','media-library','media-source','selection-person','Private',1,0,0),
 ('selection-foreign-album','selection-foreign-library','selection-foreign-source','selection-foreign-person','Foreign',0,0,0),
 ('selection-unmapped-album','media-library','media-source','selection-no-path','Unmapped',0,0,0);
 INSERT INTO tags(id,name) VALUES('selection-tag','Private assigned label'),('selection-unused','Unused');
 INSERT INTO album_tags(album_id,tag_id) VALUES('media-album','selection-tag'),('selection-other-album','selection-tag'),('selection-private-album','selection-tag'),('selection-foreign-album','selection-tag')`); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAlbumSelectionHTTPPagesBindLiteralResourcesScopeAndAccount(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	const path = "/api/v2/albums"
	query := url.Values{"library_id": {"media-library"}, "source_id": {"media-source"}, "person_id": {"selection-person"}, "tag_id": {"selection-tag"}, "limit": {"1"}}
	response := f.reader.request("GET", path+"?"+query.Encode(), nil, "", "")
	first := decodeResponse[listPage[access.Album]](t, response, 200)
	if len(first.Items) != 1 || first.Items[0].ID != "media-album" || first.Items[0].MediaCount != 1 || first.Items[0].CoverMediaID == nil || *first.Items[0].CoverMediaID != "protected" || !first.HasMore || first.NextCursor == nil || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("selected album page is not bounded/current/private", first, response.Header())
	}
	continued := url.Values{}
	for key, values := range query {
		continued[key] = slices.Clone(values)
	}
	continued.Set("cursor", *first.NextCursor)
	continued.Set("limit", "100")
	last := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+continued.Encode(), nil, "", ""), 200)
	if len(last.Items) != 1 || last.Items[0].ID != "selection-other-album" || last.Items[0].MediaCount != 0 || last.Items[0].CoverMediaID != nil || last.HasMore || last.NextCursor != nil {
		t.Fatal("filtered album keysets lost empty cards or leaked restricted albums", last)
	}
	for key := range map[string]bool{"library_id": true, "source_id": true, "person_id": true, "tag_id": true} {
		changed := url.Values{}
		for k, values := range query {
			changed[k] = slices.Clone(values)
		}
		changed.Set("cursor", *first.NextCursor)
		changed.Del(key)
		expectCode(t, f.reader.request("GET", path+"?"+changed.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	}
	expectCode(t, f.admin.request("GET", path+"?"+continued.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, f.reader.request("GET", "/api/v2/people?"+continued.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	reordered := path + "?tag_id=selection-tag&source_id=media-source&person_id=selection-person&library_id=media-library&limit=1&cursor=" + url.QueryEscape(*first.NextCursor)
	canonical := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", reordered, nil, "", ""), 200)
	if len(canonical.Items) != 1 || canonical.Items[0].ID != "selection-other-album" {
		t.Fatal("query key order changed an equivalent selection", canonical)
	}
	data, err := base64.RawURLEncoding.DecodeString(*first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	var cursor pageCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		t.Fatal(err)
	}
	cursor.Position = "unknown"
	encoded, _ := json.Marshal(cursor)
	continued.Set("cursor", base64.RawURLEncoding.EncodeToString(encoded))
	expectCode(t, f.reader.request("GET", path+"?"+continued.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	for _, filter := range []url.Values{
		{"library_id": {"unknown"}}, {"source_id": {"unknown"}}, {"person_id": {"unknown"}}, {"tag_id": {"selection-unused"}},
		{"library_id": {"selection-foreign-library"}}, {"source_id": {"selection-foreign-source"}}, {"person_id": {"selection-foreign-person"}},
		{"library_id": {"media-library"}, "source_id": {"selection-foreign-source"}}, {"person_id": {"selection-no-path"}},
	} {
		empty := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+filter.Encode(), nil, "", ""), 200)
		if empty.Items == nil || len(empty.Items) != 0 || empty.HasMore || empty.NextCursor != nil {
			t.Fatal("hidden/unknown selectors disclosed global catalog existence", filter, empty)
		}
	}
	expectCode(t, f.reader.request("GET", "/api/v2/admin/tags", nil, "", ""), 403, "ADMIN_REQUIRED")
	if _, err := f.db.Exec(`UPDATE albums SET person_id='selection-foreign-person' WHERE id='media-album'`); err != nil {
		t.Fatal(err)
	}
	foreign := decodeResponse[listPage[access.Album]](t, f.admin.request("GET", path+"?person_id=selection-foreign-person", nil, "", ""), 200)
	if len(foreign.Items) != 1 || foreign.Items[0].ID != "selection-foreign-album" {
		t.Fatal("cross-source person association disclosed another source's albums", foreign)
	}
	if _, err := f.db.Exec(`UPDATE users SET scope_id='new-album-selection-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	continued.Set("cursor", *first.NextCursor)
	expectCode(t, f.reader.request("GET", path+"?"+continued.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	for _, secret := range []string{f.source, "root_path", "relative_path", "source_sha256", "storage_path", "Private assigned label"} {
		if strings.Contains(response.Body.String()+string(data), secret) {
			t.Fatal("album selection/cursor exposed private selectors or labels", secret)
		}
	}
}

func TestAlbumSelectionHTTPFreshResourceStateAndInternalFailureStayPrivate(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	const path = "/api/v2/albums?library_id=media-library&source_id=media-source&person_id=selection-person&tag_id=selection-tag"
	for _, change := range []struct {
		apply, restore string
		hidden         bool
	}{
		{`UPDATE sources SET state='unavailable' WHERE id='media-source'`, `UPDATE sources SET state='healthy' WHERE id='media-source'`, false},
		{`UPDATE sources SET is_active=0 WHERE id='media-source'`, `UPDATE sources SET is_active=1 WHERE id='media-source'`, true},
		{`UPDATE libraries SET is_active=0 WHERE id='media-library'`, `UPDATE libraries SET is_active=1 WHERE id='media-library'`, true},
		{`UPDATE sources SET library_id='selection-foreign-library' WHERE id='media-source'`, `UPDATE sources SET library_id='media-library' WHERE id='media-source'`, true},
	} {
		if _, err := f.db.Exec(change.apply); err != nil {
			t.Fatal(err)
		}
		for _, viewer := range []struct {
			client adminClient
			want   int
		}{{f.reader, 2}, {f.admin, 3}} {
			page := decodeResponse[listPage[access.Album]](t, viewer.client.request("GET", path, nil, "", ""), 200)
			want := viewer.want
			if change.hidden {
				want = 0
			}
			if page.Items == nil || len(page.Items) != want || page.HasMore || page.NextCursor != nil {
				t.Fatal("album selection ignored current source/library safety or treated reachability as access", page, change.apply)
			}
		}
		if _, err := f.db.Exec(change.restore); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`DROP INDEX idx_album_source_page`); err != nil {
		t.Fatal(err)
	}
	failed := f.reader.request("GET", "/api/v2/albums?source_id=media-source", nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	for _, secret := range []string{"idx_album_source_page", "SQL", "albums", f.source} {
		if strings.Contains(failed.Body.String(), secret) {
			t.Fatal("album selection exposed internal SQL/source details", failed.Body.String())
		}
	}
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", "/api/v2/albums?sort=latest", nil, "", ""), 401, "UNAUTHENTICATED")
}

func TestAlbumSelectionHTTPRejectsUnsupportedSelectorsAndPreservesAnonymousDenial(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	const path = "/api/v2/albums"
	for _, field := range []string{"library_id", "source_id", "person_id", "tag_id"} {
		for _, value := range []string{"", " padded", "/private/path", "bad\x00", strings.Repeat("a", 65), "bad" + string(rune(0x200d)), string([]byte{0xff})} {
			expectCode(t, f.reader.request("GET", path+"?"+url.Values{field: {value}}.Encode(), nil, "", ""), 400, "INVALID_FILTER")
		}
		expectCode(t, f.reader.request("GET", path+"?"+field+"=one&"+field+"=two", nil, "", ""), 400, "INVALID_FILTER")
	}
	for _, query := range []string{"q=anything", "sort=popular", "kind=image", "library_id[]=%20", "Person_ID=selection-person", "tag_ids=one,two"} {
		expectCode(t, f.reader.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_FILTER")
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "cursor=x&cursor=y", "limit=%zz"} {
		expectCode(t, f.reader.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	expectCode(t, f.reader.request("GET", path+"?cursor=", nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, (adminClient{handler: f.http}).request("GET", path+"?q=anything", nil, "", ""), 401, "UNAUTHENTICATED")
	quoted := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?tag_id="+url.QueryEscape("literal' OR 1=1"), nil, "", ""), 200)
	if len(quoted.Items) != 0 {
		t.Fatal("resource selector was interpolated into SQL", quoted)
	}
}
