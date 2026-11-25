package httpapi

import (
	"net/url"
	"slices"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func TestFavoriteSubtreeSelectionHTTP(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	var adminID string
	if err := f.db.QueryRow(`SELECT id FROM users WHERE role='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at,updated_at) VALUES
 ('tree-root',?,NULL,'Root',0,0),('tree-child',?,'tree-root','Child',0,0),
 ('tree-grandchild',?,'tree-child','Grandchild',0,0),('tree-other',?,NULL,'Other',0,0),
 ('tree-foreign',?,NULL,'Foreign',0,0)`, f.user.ID, f.user.ID, f.user.ID, f.user.ID, adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES
 ('tree-outside','media-library','media-source','Outside',1,1),
 ('tree-cleared','media-library','media-source','Cleared',2,2)`); err != nil {
		t.Fatal(err)
	}
	favorite := func(client adminClient, album string, folder any) string {
		t.Helper()
		path := "/api/v2/favorites/" + album
		initial := client.request("GET", path, nil, "", "")
		decodeResponse[access.AlbumFavorite](t, initial, 200)
		changed := client.request("PUT", path, map[string]any{"collection_id": folder}, initial.Header().Get("ETag"), store.NewID())
		decodeResponse[access.AlbumFavorite](t, changed, 200)
		return changed.Header().Get("ETag")
	}
	favorite(f.reader, "media-album", "tree-root")
	childTag := favorite(f.reader, "selection-other-album", "tree-child")
	favorite(f.reader, "selection-unmapped-album", "tree-grandchild")
	favorite(f.reader, "tree-outside", "tree-other")
	clearTag := favorite(f.reader, "tree-cleared", "tree-child")
	decodeResponse[access.AlbumFavorite](t, f.reader.request("DELETE", "/api/v2/favorites/tree-cleared", nil, clearTag, store.NewID()), 200)
	favorite(f.admin, "media-album", "tree-foreign")
	if _, err := f.db.Exec(`UPDATE albums SET access_restricted=0 WHERE id='selection-private-album'`); err != nil {
		t.Fatal(err)
	}
	favorite(f.reader, "selection-private-album", "tree-grandchild")
	if _, err := f.db.Exec(`UPDATE albums SET access_restricted=1 WHERE id='selection-private-album'`); err != nil {
		t.Fatal(err)
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
			t.Fatal("favorite subtree returned the wrong visible memberships", query, got, want, result)
		}
	}
	ids(f.reader, url.Values{"favorite_collection_id": {"tree-root"}}, "media-album")
	root := url.Values{"favorite_collection_id": {"tree-root"}, "favorite_include_descendants": {"true"}}
	ids(f.reader, root, "media-album", "selection-other-album", "selection-unmapped-album")
	ids(f.reader, url.Values{"favorite_collection_id": {"tree-child"}, "favorite_include_descendants": {"true"}}, "selection-other-album", "selection-unmapped-album")
	ids(f.reader, url.Values{"favorite_collection_id": {"tree-grandchild"}, "favorite_include_descendants": {"true"}}, "selection-unmapped-album")
	ids(f.reader, url.Values{"favorite_collection_id": {"tree-root"}, "favorite_include_descendants": {"true"}, "favorite": {"true"}, "tag_id": {"selection-tag"}}, "media-album", "selection-other-album")
	ids(f.reader, url.Values{"favorite_collection_id": {"tree-foreign"}, "favorite_include_descendants": {"true"}})
	ids(f.reader, url.Values{"favorite_collection_id": {"tree-missing"}, "favorite_include_descendants": {"true"}})
	ids(f.admin, url.Values{"favorite_collection_id": {"tree-root"}, "favorite_include_descendants": {"true"}})
	ids(f.admin, url.Values{"favorite_collection_id": {"tree-foreign"}, "favorite_include_descendants": {"true"}}, "media-album")

	var firstCursor string
	for _, order := range []string{"", "latest", "oldest", "name_asc", "name_desc", "top_rated", "popular"} {
		query := url.Values{"favorite_collection_id": {"tree-root"}, "favorite_include_descendants": {"true"}, "limit": {"1"}}
		if order != "" {
			query.Set("sort", order)
		}
		var seen []string
		for i := 0; i < 3; i++ {
			result := page(f.reader, query)
			if len(result.Items) != 1 || result.HasMore != (i < 2) || (result.NextCursor != nil) != (i < 2) {
				t.Fatal("favorite subtree did not produce bounded keyset pages", order, i, result)
			}
			seen = append(seen, result.Items[0].ID)
			if result.NextCursor != nil {
				if order == "" && i == 0 {
					firstCursor = *result.NextCursor
				}
				query.Set("cursor", *result.NextCursor)
			}
		}
		slices.Sort(seen)
		if !slices.Equal(seen, []string{"media-album", "selection-other-album", "selection-unmapped-album"}) {
			t.Fatal("favorite subtree paging repeated or omitted albums", order, seen)
		}
	}
	continuation := url.Values{"favorite_collection_id": {"tree-root"}, "favorite_include_descendants": {"true"}, "cursor": {firstCursor}}
	expectCode(t, f.admin.request("GET", path+continuation.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	direct := url.Values{"favorite_collection_id": {"tree-root"}, "cursor": {firstCursor}}
	expectCode(t, f.reader.request("GET", path+direct.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	otherRoot := url.Values{"favorite_collection_id": {"tree-child"}, "favorite_include_descendants": {"true"}, "cursor": {firstCursor}}
	expectCode(t, f.reader.request("GET", path+otherRoot.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	moved := f.reader.request("PUT", "/api/v2/favorites/selection-other-album", map[string]any{"collection_id": "tree-other"}, childTag, store.NewID())
	decodeResponse[access.AlbumFavorite](t, moved, 200)
	ids(f.reader, continuation, "selection-unmapped-album")
	favorite(f.reader, "tree-outside", "tree-root")
	directPage := page(f.reader, url.Values{"favorite_collection_id": {"tree-root"}, "limit": {"1"}})
	if !directPage.HasMore || directPage.NextCursor == nil {
		t.Fatal("direct-folder fixture did not produce a continuation", directPage)
	}
	changedMode := url.Values{"favorite_collection_id": {"tree-root"}, "favorite_include_descendants": {"true"}, "cursor": {*directPage.NextCursor}}
	expectCode(t, f.reader.request("GET", path+changedMode.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	for _, invalid := range []string{
		"favorite_include_descendants=true", "favorite_include_descendants=false&favorite_collection_id=tree-root",
		"favorite_include_descendants=&favorite_collection_id=tree-root", "favorite_include_descendants=TRUE&favorite_collection_id=tree-root",
		"favorite_include_descendants=true&favorite_include_descendants=true&favorite_collection_id=tree-root",
		"favorite_include_descendants=true&favorite_collection_id=tree-root&favorite=false",
		"favorite_include_descendants=true&favorite_collection_id=tree-root&favorite_unfiled=true",
	} {
		expectCode(t, f.reader.request("GET", path+invalid, nil, "", ""), 400, "INVALID_FILTER")
	}
	if _, err := f.db.Exec(`DELETE FROM user_groups WHERE user_id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	ids(f.reader, root)
	if _, err := f.db.Exec(`UPDATE users SET scope_id='tree-new-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+continuation.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, (adminClient{handler: f.http}).request("GET", path+root.Encode(), nil, "", ""), 401, "UNAUTHENTICATED")
}
