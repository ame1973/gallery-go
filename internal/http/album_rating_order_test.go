package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func albumRatingOrderHTTPFixture(t *testing.T) (protectedFixture, auth.User) {
	t.Helper()
	f := albumOrderHTTPFixture(t)
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES
 ('selection-zero-album','media-library','media-source','selection-person','Empty zero',0,0),
 ('selection-null-album','media-library','media-source','selection-person','Empty null',0,0);
 INSERT INTO album_tags VALUES('selection-zero-album','selection-tag'),('selection-null-album','selection-tag')`); err != nil {
		t.Fatal(err)
	}
	third := createHTTPUser(t, f.admin, "rank-third-voter", "user")
	decodeResponse[access.UserGroups](t, f.admin.request("PUT", "/api/v2/admin/users/"+third.ID+"/groups", map[string]any{"group_ids": []string{"media-group"}}, userETag(third), ""), 200)
	thirdClient := accountClient(t, f.http, third.Username, testPassword)
	for _, vote := range []struct {
		client adminClient
		id     string
		value  int
	}{
		{f.admin, "media-album", 1}, {f.reader, "media-album", 4}, {thirdClient, "media-album", 5},
		{f.admin, "selection-other-album", 5}, {f.admin, "selection-tie-album", 5},
		{f.reader, "selection-tie-album", 5}, {f.admin, "selection-private-album", 5},
	} {
		path := "/api/v2/albums/" + vote.id + "/rating"
		current := vote.client.request("GET", path, nil, "", "")
		decodeResponse[access.AlbumRating](t, current, 200)
		decodeResponse[access.AlbumRating](t, vote.client.request("PUT", path, map[string]int{"rating": vote.value}, current.Header().Get("ETag"), store.NewID()), 200)
	}
	return f, third
}

func TestAlbumRatingOrderHTTPFreezesFullPublicMeanAndKeepsScopeAndSharedCards(t *testing.T) {
	f, _ := albumRatingOrderHTTPFixture(t)
	const path = "/api/v2/albums"
	query := url.Values{"library_id": {"media-library"}, "source_id": {"media-source"}, "person_id": {"selection-person"}, "tag_id": {"selection-tag"}, "sort": {"top_rated"}, "limit": {"1"}}
	want := []string{"selection-tie-album", "selection-other-album", "media-album", "selection-zero-album", "selection-null-album"}
	seen := []string{}
	firstCursor := ""
	var first pageCursor
	for range 6 {
		response := f.reader.request("GET", path+"?"+query.Encode(), nil, "", "")
		page := decodeResponse[listPage[access.Album]](t, response, 200)
		if len(page.Items) != 1 || response.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("ranked page was not bounded/authorized/private", page, response.Header())
		}
		album := page.Items[0]
		seen = append(seen, album.ID)
		detail := decodeResponse[access.Album](t, f.reader.request("GET", path+"/"+album.ID, nil, "", ""), 200)
		if !reflect.DeepEqual(album, detail) {
			t.Fatal("ranked card differs from common authorized detail", album, detail)
		}
		if album.ID == "media-album" && (album.RatingAvg == nil || math.Float64bits(*album.RatingAvg) != math.Float64bits(10.0/3)) {
			t.Fatal("published repeating native mean was rounded", album)
		}
		if page.NextCursor == nil {
			if page.HasMore {
				t.Fatal("ranked terminal page lost its continuation")
			}
			break
		}
		data, err := base64.RawURLEncoding.DecodeString(*page.NextCursor)
		var cursor pageCursor
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Position != access.AlbumRatingKey(album.RatingAvg) || len(cursor.Position) > 32 || cursor.After != album.ID {
			t.Fatal("ranked cursor changed/truncated the complete public mean", cursor, album, err)
		}
		for _, secret := range []string{f.source, "root_path", "relative_path", "source_sha256", "storage_path", "user_id", "rating_count"} {
			if strings.Contains(response.Body.String()+string(data), secret) {
				t.Fatal("ranked metadata exposed private data or a hidden tie criterion", secret)
			}
		}
		if firstCursor == "" {
			firstCursor, first = *page.NextCursor, cursor
		}
		query.Set("cursor", *page.NextCursor)
	}
	if !slices.Equal(seen, want) {
		t.Fatal("ranked pages lost equal means, nulls or filtered hidden cards after LIMIT", seen, want)
	}
	query.Set("cursor", firstCursor)
	expectCode(t, f.admin.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("sort", "name_desc")
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	query.Set("sort", "top_rated")
	for _, key := range []string{"", "5", "n:NQ", "r:NaN", "r:+Inf", "r:1e0", "r:1.0", "r:-0", "r:0.5", "r:6", "r:" + strings.Repeat("1", 33)} {
		invalid := first
		invalid.Position = key
		data, _ := json.Marshal(invalid)
		query.Set("cursor", base64.RawURLEncoding.EncodeToString(data))
		expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	}
	query.Del("cursor")
	if _, err := f.db.Exec(`DROP INDEX idx_album_person_rating_page`); err != nil {
		t.Fatal(err)
	}
	failed := f.reader.request("GET", path+"?"+query.Encode(), nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	for _, secret := range []string{"idx_album", "SQL", f.source} {
		if strings.Contains(problemContent(t, failed), secret) {
			t.Fatal("rank query failure exposed private database/source details", failed.Body.String())
		}
	}
}

func TestAlbumRatingOrderHTTPRealRerateAccountDeleteAndDeletedAnchorKeepFrozenKey(t *testing.T) {
	f, third := albumRatingOrderHTTPFixture(t)
	const path = "/api/v2/albums"
	query := url.Values{"source_id": {"media-source"}, "tag_id": {"selection-tag"}, "sort": {"top_rated"}, "limit": {"1"}}
	first := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if first.NextCursor == nil || len(first.Items) != 1 || first.Items[0].ID != "selection-tie-album" {
		t.Fatal(first)
	}
	ratingPath := "/api/v2/albums/selection-tie-album/rating"
	personal := f.admin.request("GET", ratingPath, nil, "", "")
	decodeResponse[access.AlbumRating](t, f.admin.request("PUT", ratingPath, map[string]int{"rating": 1}, personal.Header().Get("ETag"), store.NewID()), 200)
	query.Set("cursor", *first.NextCursor)
	next := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if len(next.Items) != 1 || next.Items[0].ID != "selection-other-album" {
		t.Fatal("HTTP continuation reread the anchor's newly lowered mean", next)
	}
	userPath := "/api/v2/admin/users/" + third.ID
	user := f.admin.request("GET", userPath, nil, "", "")
	if response := f.admin.request("DELETE", userPath, nil, user.Header().Get("ETag"), ""); response.Code != 204 {
		t.Fatal("real account delete failed", response.Code, response.Body.String())
	}
	query.Del("cursor")
	query.Set("limit", "100")
	current := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	want := []string{"selection-other-album", "selection-tie-album", "media-album", "selection-zero-album", "selection-null-album"}
	ids := []string{}
	for _, item := range current.Items {
		ids = append(ids, item.ID)
	}
	if !slices.Equal(ids, want) || current.Items[2].RatingAvg == nil || *current.Items[2].RatingAvg != 2.5 {
		t.Fatal("account deletion did not update the actual indexed native rank", current, want)
	}
	// Delete only this fixture's empty card, with a real vote FK cascade.
	if _, err := f.db.Exec(`DELETE FROM albums WHERE id='selection-tie-album'`); err != nil {
		t.Fatal(err)
	}
	query.Set("cursor", *first.NextCursor)
	query.Set("limit", "1")
	next = decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 200)
	if len(next.Items) != 1 || next.Items[0].ID != "selection-other-album" {
		t.Fatal("rank continuation required its deleted anchor", next)
	}
	if _, err := f.db.Exec(`UPDATE users SET scope_id='new-rating-order-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?"+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
}
