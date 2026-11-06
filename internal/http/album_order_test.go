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

func albumOrderHTTPFixture(t *testing.T) protectedFixture {
	t.Helper()
	f := albumSelectionHTTPFixture(t)
	if _, err := f.db.Exec(`UPDATE albums SET created_at=CASE id WHEN 'selection-other-album' THEN -1000 WHEN 'selection-private-album' THEN 1000 ELSE 0 END;
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES('selection-tie-album','media-library','media-source','selection-person','Empty tie',0,0);
 INSERT INTO album_tags VALUES('selection-tie-album','selection-tag')`); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAlbumOrderHTTPUsesPublicTimeTuplesAndSharedCardsBeforeLimit(t *testing.T) {
	f := albumOrderHTTPFixture(t)
	const path = "/api/v2/albums"
	for _, order := range []string{"oldest", "latest"} {
		query := url.Values{"library_id": {"media-library"}, "source_id": {"media-source"}, "person_id": {"selection-person"}, "tag_id": {"selection-tag"}, "sort": {order}, "limit": {"1"}}
		want := []string{"selection-other-album", "media-album", "selection-tie-album"}
		if order == "latest" {
			slices.Reverse(want)
		}
		seen := []string{}
		for range 4 {
			response := f.reader.request("GET", path+"?"+query.Encode(), nil, "", "")
			page := decodeResponse[listPage[access.Album]](t, response, 200)
			if len(page.Items) != 1 || response.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(response.Body.String(), `"indexed_at":`) {
				t.Fatal("ordered page is not a bounded private public-time DTO", page, response.Body.String())
			}
			album := page.Items[0]
			seen = append(seen, album.ID)
			detail := decodeResponse[access.Album](t, f.reader.request("GET", path+"/"+album.ID, nil, "", ""), 200)
			one, _ := json.Marshal(album)
			two, _ := json.Marshal(detail)
			if string(one) != string(two) || (album.ID == "media-album" && (album.MediaCount != 1 || album.CoverMediaID == nil)) {
				t.Fatal("chronological card differs from shared authorized detail", album, detail)
			}
			if page.NextCursor == nil {
				if page.HasMore {
					t.Fatal("ordered terminal page advertises a missing continuation", page)
				}
				break
			}
			data, err := base64.RawURLEncoding.DecodeString(*page.NextCursor)
			var cursor pageCursor
			if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.After != album.ID || cursor.Position == "" {
				t.Fatal("chronological cursor did not freeze its public key", string(data), err)
			}
			for _, secret := range []string{"root_path", "relative_path", "source_sha256", "storage_path", f.source} {
				if strings.Contains(response.Body.String()+string(data), secret) {
					t.Fatal("chronological page leaked a private source key", secret)
				}
			}
			query.Set("cursor", *page.NextCursor)
			query.Set("limit", "1")
		}
		if !slices.Equal(seen, want) {
			t.Fatal("chronological pages lost ties, negative time or applied ACL after limit", order, seen, want)
		}
	}
	people := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", "/api/v2/people/selection-person/albums", nil, "", ""), 200)
	if len(people.Items) != 3 || people.Items[0].ID != "media-album" || !people.Items[0].IndexedAt.Equal(decodeResponse[access.Album](t, f.reader.request("GET", path+"/media-album", nil, "", ""), 200).IndexedAt) {
		t.Fatal("PersonAlbums did not retain the common indexed_at projection", people)
	}
}

func TestAlbumOrderHTTPValidatesFrozenPositionsAndLiveScopeWithoutAnchorLookup(t *testing.T) {
	f := albumOrderHTTPFixture(t)
	const path = "/api/v2/albums"
	query := url.Values{"tag_id": {"selection-tag"}, "source_id": {"media-source"}, "sort": {"latest"}, "limit": {"1"}}
	first := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if len(first.Items) != 1 || first.Items[0].ID != "selection-tie-album" || first.NextCursor == nil {
		t.Fatal(first)
	}
	data, err := base64.RawURLEncoding.DecodeString(*first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	var cursor pageCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.Position != "0" {
		t.Fatal("cursor public time differs from indexed_at", cursor, err)
	}
	for _, key := range []string{"", "unknown", "01", "+0", "-0", "1.5", "1e3", "253402300800000", "-62135596800001"} {
		invalid := cursor
		invalid.Position = key
		encoded, _ := json.Marshal(invalid)
		query.Set("cursor", base64.RawURLEncoding.EncodeToString(encoded))
		expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	}
	query.Set("cursor", *first.NextCursor)
	expectCode(t, f.admin.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("sort", "oldest")
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Del("sort")
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("sort", "latest")
	if _, err := f.db.Exec(`DELETE FROM albums WHERE id='selection-tie-album'`); err != nil {
		t.Fatal(err)
	}
	query.Set("limit", "100")
	last := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if len(last.Items) != 2 || last.Items[0].ID != "media-album" || last.Items[1].ID != "selection-other-album" || last.HasMore || last.NextCursor != nil {
		t.Fatal("deleted anchor invalidated or retargeted the frozen continuation", last)
	}
	if _, err := f.db.Exec(`UPDATE users SET scope_id='new-chronological-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
}

func TestAlbumOrderHTTPRejectsUnimplementedAmbiguousSortsAndInvalidStoredTime(t *testing.T) {
	f := albumOrderHTTPFixture(t)
	for _, query := range []string{"sort=", "sort=latest&sort=oldest", "sort=LATEST", "sort=name_asc", "sort=popular", "sort=top_rated", "sort=id_asc"} {
		expectCode(t, f.reader.request("GET", "/api/v2/albums?"+query, nil, "", ""), 400, "INVALID_FILTER")
	}
	if _, err := f.db.Exec(`UPDATE albums SET created_at=253402300800000 WHERE id='media-album'`); err != nil {
		t.Fatal(err)
	}
	failed := f.reader.request("GET", "/api/v2/albums?source_id=media-source&sort=latest", nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	for _, secret := range []string{"253402300800000", "created_at", "SQL", f.source} {
		if strings.Contains(failed.Body.String(), secret) {
			t.Fatal("invalid stored indexed time leaked internal evidence", failed.Body.String())
		}
	}
}
