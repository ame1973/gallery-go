package httpapi

import (
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func TestFavoriteFolderSelectionHTTPIsolationCursorsAndLiveMutations(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	var adminID string
	if err := f.db.QueryRow(`SELECT id FROM users WHERE role='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at,updated_at) VALUES
 ('folder-root',?,NULL,'Same',0,0),('folder-child',?,'folder-root','Child',0,0),('folder-foreign',?,NULL,'Same',0,0)`, f.user.ID, f.user.ID, adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES
 ('selection-folder-extra','media-library','media-source','selection-person','Extra',1,1),
 ('selection-folder-default','media-library','media-source','selection-person','Default',2,2),
 ('selection-folder-cleared','media-library','media-source','selection-person','Cleared',3,3);
 INSERT INTO album_tags(album_id,tag_id) VALUES('selection-folder-extra','selection-tag'),('selection-folder-default','selection-tag'),('selection-folder-cleared','selection-tag')`); err != nil {
		t.Fatal(err)
	}
	favorite := func(client adminClient, id string, collection any) (string, string, string) {
		t.Helper()
		path := "/api/v2/favorites/" + id
		initial := client.request("GET", path, nil, "", "")
		decodeResponse[access.AlbumFavorite](t, initial, 200)
		key := store.NewID()
		changed := client.request("PUT", path, map[string]any{"collection_id": collection}, initial.Header().Get("ETag"), key)
		value := decodeResponse[access.AlbumFavorite](t, changed, 200)
		if !value.Favorited || value.Version != 2 {
			t.Fatal("favorite fixture did not create a live membership", value)
		}
		return initial.Header().Get("ETag"), changed.Header().Get("ETag"), key
	}
	originalTag, currentTag, originalKey := favorite(f.reader, "media-album", "folder-root")
	_, otherTag, _ := favorite(f.reader, "selection-other-album", "folder-root")
	favorite(f.reader, "selection-folder-extra", "folder-root")
	favorite(f.reader, "selection-unmapped-album", "folder-child")
	favorite(f.reader, "selection-folder-default", nil)
	_, clearTag, _ := favorite(f.reader, "selection-folder-cleared", nil)
	favorite(f.admin, "media-album", "folder-foreign")
	if _, err := f.db.Exec(`UPDATE albums SET access_restricted=0 WHERE id='selection-private-album'`); err != nil {
		t.Fatal(err)
	}
	favorite(f.reader, "selection-private-album", "folder-root")
	if _, err := f.db.Exec(`UPDATE albums SET access_restricted=1 WHERE id='selection-private-album'`); err != nil {
		t.Fatal(err)
	}
	cleared := decodeResponse[access.AlbumFavorite](t, f.reader.request("DELETE", "/api/v2/favorites/selection-folder-cleared", nil, clearTag, store.NewID()), 200)
	if cleared.Favorited || cleared.Version != 3 {
		t.Fatal("fixture clear did not retain its personal revision", cleared)
	}
	const path = "/api/v2/albums?"
	page := func(client adminClient, query url.Values) listPage[access.Album] {
		t.Helper()
		return decodeResponse[listPage[access.Album]](t, client.request("GET", path+query.Encode(), nil, "", ""), 200)
	}
	ids := func(client adminClient, query url.Values, want ...string) {
		t.Helper()
		result := page(client, query)
		got := make([]string, 0, len(result.Items))
		for _, item := range result.Items {
			got = append(got, item.ID)
		}
		if result.Items == nil || !slices.Equal(got, want) || result.HasMore || result.NextCursor != nil {
			t.Fatal("folder selection returned an incorrect or unbounded page", query, got, want, result)
		}
	}
	root := url.Values{"favorite_collection_id": {"folder-root"}, "library_id": {"media-library"}, "source_id": {"media-source"}, "person_id": {"selection-person"}, "tag_id": {"selection-tag"}}
	ids(f.reader, root, "media-album", "selection-folder-extra", "selection-other-album")
	ids(f.reader, url.Values{"favorite_collection_id": {"folder-child"}}, "selection-unmapped-album")
	ids(f.reader, url.Values{"favorite_unfiled": {"true"}}, "selection-folder-default")
	ids(f.reader, url.Values{"favorite_unfiled": {"true"}, "favorite": {"true"}}, "selection-folder-default")
	ids(f.reader, url.Values{"favorite_collection_id": {"folder-foreign"}})
	ids(f.reader, url.Values{"favorite_collection_id": {"missing-folder"}})
	ids(f.admin, url.Values{"favorite_collection_id": {"folder-root"}})
	ids(f.admin, url.Values{"favorite_collection_id": {"folder-foreign"}}, "media-album")
	ids(f.reader, url.Values{"favorite_collection_id": {"literal' OR 1=1"}})

	// Each cursor freezes the ordered key and binds the actual actor, scope and filters.
	var firstCursor string
	for _, order := range []string{"", "latest", "oldest", "name_asc", "name_desc", "top_rated"} {
		query := url.Values{}
		for key, values := range root {
			query[key] = slices.Clone(values)
		}
		query.Set("limit", "1")
		if order != "" {
			query.Set("sort", order)
		}
		var seen []string
		for index := 0; index < 3; index++ {
			result := page(f.reader, query)
			if len(result.Items) != 1 || result.HasMore != (index < 2) || (result.NextCursor != nil) != (index < 2) {
				t.Fatal("folder keyset did not produce bounded pages", order, index, result)
			}
			seen = append(seen, result.Items[0].ID)
			if result.NextCursor != nil {
				if order == "" && index == 0 {
					firstCursor = *result.NextCursor
				}
				query.Set("cursor", *result.NextCursor)
			}
		}
		slices.Sort(seen)
		if !slices.Equal(seen, []string{"media-album", "selection-folder-extra", "selection-other-album"}) {
			t.Fatal("ordered folder pages repeated or omitted membership", order, seen)
		}
	}
	continuation := url.Values{}
	for key, values := range root {
		continuation[key] = slices.Clone(values)
	}
	continuation.Set("cursor", firstCursor)
	continuation.Set("limit", "100")
	expectCode(t, f.admin.request("GET", path+continuation.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	for _, changed := range []struct{ field, value string }{{"favorite_collection_id", "folder-child"}, {"favorite", "true"}, {"tag_id", "selection-unused"}} {
		query := url.Values{}
		for key, values := range continuation {
			query[key] = slices.Clone(values)
		}
		query.Set(changed.field, changed.value)
		expectCode(t, f.reader.request("GET", path+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	}
	query := url.Values{}
	for key, values := range continuation {
		query[key] = slices.Clone(values)
	}
	query.Del("favorite_collection_id")
	query.Set("favorite_unfiled", "true")
	expectCode(t, f.reader.request("GET", path+query.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	before := decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	moved := f.reader.request("PUT", "/api/v2/favorites/media-album", map[string]any{"collection_id": "folder-child"}, currentTag, store.NewID())
	if value := decodeResponse[access.AlbumFavorite](t, moved, 200); value.Version != 3 || value.CollectionID == nil || *value.CollectionID != "folder-child" {
		t.Fatal("moving a live membership did not update its direct folder", value)
	}
	ids(f.reader, continuation, "selection-folder-extra", "selection-other-album")
	ids(f.reader, url.Values{"favorite_collection_id": {"folder-child"}}, "media-album", "selection-unmapped-album")
	replay := decodeResponse[access.AlbumFavorite](t, f.reader.request("PUT", "/api/v2/favorites/media-album", map[string]any{"collection_id": "folder-root"}, originalTag, originalKey), 200)
	if replay.Version != 2 || replay.CollectionID == nil || *replay.CollectionID != "folder-root" {
		t.Fatal("favorite receipt did not return its historical assignment", replay)
	}
	ids(f.reader, root, "selection-folder-extra", "selection-other-album")
	movedDefault := f.reader.request("PUT", "/api/v2/favorites/selection-other-album", map[string]any{"collection_id": nil}, otherTag, store.NewID())
	decodeResponse[access.AlbumFavorite](t, movedDefault, 200)
	ids(f.reader, url.Values{"favorite_unfiled": {"true"}}, "selection-folder-default", "selection-other-album")
	decodeResponse[access.AlbumFavorite](t, f.reader.request("DELETE", "/api/v2/favorites/media-album", nil, moved.Header().Get("ETag"), store.NewID()), 200)
	ids(f.reader, url.Values{"favorite_unfiled": {"true"}}, "selection-folder-default", "selection-other-album")
	ids(f.reader, url.Values{"favorite_collection_id": {"folder-child"}}, "selection-unmapped-album")
	after := decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("personal folder moves changed shared album metadata", before, after)
	}
	if _, err := f.db.Exec(`UPDATE users SET download_original=0 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, mediaRequest(f.reader, "GET", f.originalURL, nil), 404, "NOT_FOUND")
	response := f.reader.request("GET", path+root.Encode(), nil, "", "")
	for _, secret := range []string{f.source, "root_path", "relative_path", "source_sha256", "folder-root", "Private assigned label"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("folder page exposed private source or collection metadata", secret)
		}
	}
	for _, invalid := range []string{
		"favorite_unfiled=", "favorite_unfiled=false", "favorite_unfiled=TRUE", "favorite_unfiled=1", "favorite_unfiled=true&favorite_unfiled=true",
		"favorite_collection_id=", "favorite_collection_id=one&favorite_collection_id=two", "favorite_collection_id=one&favorite_unfiled=true",
		"favorite=false&favorite_collection_id=one", "favorite=false&favorite_unfiled=true", "user_id=other&favorite_unfiled=true",
	} {
		expectCode(t, f.reader.request("GET", path+invalid, nil, "", ""), 400, "INVALID_FILTER")
	}
	expectCode(t, (adminClient{handler: f.http}).request("GET", path+"favorite_unfiled=true", nil, "", ""), 401, "UNAUTHENTICATED")
	if _, err := f.db.Exec(`DELETE FROM user_groups WHERE user_id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	ids(f.reader, root)
	ids(f.reader, url.Values{"favorite_unfiled": {"true"}})
	if _, err := f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES(?,'media-group')`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ apply, restore string }{
		{`UPDATE sources SET is_active=0 WHERE id='media-source'`, `UPDATE sources SET is_active=1 WHERE id='media-source'`},
		{`UPDATE libraries SET is_active=0 WHERE id='media-library'`, `UPDATE libraries SET is_active=1 WHERE id='media-library'`},
		{`UPDATE sources SET library_id='selection-foreign-library' WHERE id='media-source'`, `UPDATE sources SET library_id='media-library' WHERE id='media-source'`},
	} {
		if _, err := f.db.Exec(change.apply); err != nil {
			t.Fatal(err)
		}
		ids(f.reader, root)
		ids(f.reader, url.Values{"favorite_unfiled": {"true"}})
		if _, err := f.db.Exec(change.restore); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(f.source, f.source+"-offline"); err != nil {
		t.Fatal(err)
	}
	ids(f.reader, root, "selection-folder-extra")
	ids(f.reader, url.Values{"favorite_unfiled": {"true"}}, "selection-folder-default", "selection-other-album")
	if _, err := f.db.Exec(`UPDATE users SET scope_id='folder-selection-new-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+continuation.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
}
