package httpapi

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func TestFavoriteAlbumSelectionHTTPActorCursorsLiveMutationsAndFreshVisibility(t *testing.T) {
	f := albumSelectionHTTPFixture(t)
	if _, err := f.db.Exec(`INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES('selection-http-one',?,'One',0),('selection-http-two',?,'Two',0)`, f.user.ID, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO album_tags(album_id,tag_id) VALUES('selection-unmapped-album','selection-tag')`); err != nil {
		t.Fatal(err)
	}
	favorite := func(client adminClient, id string, collection any) (string, string) {
		t.Helper()
		path := "/api/v2/favorites/" + id
		initial := client.request("GET", path, nil, "", "")
		decodeResponse[access.AlbumFavorite](t, initial, 200)
		key := store.NewID()
		body := map[string]any{"collection_id": collection}
		changed := client.request("PUT", path, body, initial.Header().Get("ETag"), key)
		value := decodeResponse[access.AlbumFavorite](t, changed, 200)
		if !value.Favorited || value.Version != 2 {
			t.Fatal("favorite producer failed", value)
		}
		return initial.Header().Get("ETag"), key
	}
	favorite(f.reader, "media-album", "selection-http-one")
	otherTag, otherKey := favorite(f.reader, "selection-other-album", "selection-http-two")
	favorite(f.admin, "selection-private-album", nil)

	query := url.Values{"library_id": {"media-library"}, "source_id": {"media-source"}, "tag_id": {"selection-tag"}, "favorite": {"true"}, "limit": {"1"}}
	path := "/api/v2/albums?"
	response := f.reader.request("GET", path+query.Encode(), nil, "", "")
	first := decodeResponse[listPage[access.Album]](t, response, 200)
	if len(first.Items) != 1 || first.Items[0].ID != "media-album" || !first.HasMore || first.NextCursor == nil || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("first favorite page lost privacy, ID order, or cross-folder membership", first, response.Header())
	}
	continued := url.Values{}
	for key, values := range query {
		continued[key] = slices.Clone(values)
	}
	continued.Set("cursor", *first.NextCursor)
	continued.Set("limit", "100")
	last := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+continued.Encode(), nil, "", ""), 200)
	if len(last.Items) != 1 || last.Items[0].ID != "selection-other-album" || last.HasMore || last.NextCursor != nil {
		t.Fatal("favorite keyset lost an owned collection or included hidden/other-account state", last)
	}
	for _, changed := range []string{"false", ""} {
		altered := url.Values{}
		for key, values := range continued {
			altered[key] = slices.Clone(values)
		}
		if changed == "" {
			altered.Del("favorite")
		} else {
			altered.Set("favorite", changed)
		}
		expectCode(t, f.reader.request("GET", path+altered.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	}
	expectCode(t, f.admin.request("GET", path+continued.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	reordered := "favorite=%74rue&tag_id=selection-tag&source_id=media-source&library_id=media-library&limit=1&cursor=" + url.QueryEscape(*first.NextCursor)
	canonical := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+reordered, nil, "", ""), 200)
	if len(canonical.Items) != 1 || canonical.Items[0].ID != "selection-other-album" {
		t.Fatal("equivalent URL encoding or query order changed the bound selector", canonical)
	}
	for _, order := range []string{"latest", "oldest", "name_asc", "name_desc", "top_rated"} {
		ordered := url.Values{}
		for key, values := range query {
			ordered[key] = slices.Clone(values)
		}
		ordered.Set("sort", order)
		one := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+ordered.Encode(), nil, "", ""), 200)
		if len(one.Items) != 1 || one.NextCursor == nil || !one.HasMore {
			t.Fatal("favorite sort lost its first bounded page", order, one)
		}
		ordered.Set("cursor", *one.NextCursor)
		two := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+ordered.Encode(), nil, "", ""), 200)
		if len(two.Items) != 1 || two.NextCursor != nil || two.HasMore {
			t.Fatal("favorite sort lost its continuation", order, two)
		}
		ids := []string{one.Items[0].ID, two.Items[0].ID}
		slices.Sort(ids)
		if !slices.Equal(ids, []string{"media-album", "selection-other-album"}) {
			t.Fatal("favorite sort repeated or omitted a live membership", order, ids)
		}
		ordered.Set("favorite", "false")
		expectCode(t, f.reader.request("GET", path+ordered.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	}
	anchorPath := "/api/v2/favorites/media-album"
	anchor := f.reader.request("GET", anchorPath, nil, "", "")
	removed := f.reader.request("DELETE", anchorPath, nil, anchor.Header().Get("ETag"), store.NewID())
	decodeResponse[access.AlbumFavorite](t, removed, 200)
	frozen := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+continued.Encode(), nil, "", ""), 200)
	if len(frozen.Items) != 1 || frozen.Items[0].ID != "selection-other-album" {
		t.Fatal("continuation reread a cleared favorite anchor", frozen)
	}
	readded := f.reader.request("PUT", anchorPath, map[string]any{"collection_id": "selection-http-one"}, removed.Header().Get("ETag"), store.NewID())
	decodeResponse[access.AlbumFavorite](t, readded, 200)
	falsePage := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"source_id=media-source&tag_id=selection-tag&favorite=false", nil, "", ""), 200)
	if len(falsePage.Items) != 1 || falsePage.Items[0].ID != "selection-unmapped-album" {
		t.Fatal("never-favorited album or another account's favorite was treated as this account's live favorite", falsePage)
	}
	adminPage := decodeResponse[listPage[access.Album]](t, f.admin.request("GET", path+"tag_id=selection-tag&favorite=true", nil, "", ""), 200)
	if len(adminPage.Items) != 1 || adminPage.Items[0].ID != "selection-private-album" {
		t.Fatal("administrator selection included another account's favorites", adminPage)
	}

	favoritePath := "/api/v2/favorites/selection-other-album"
	current := f.reader.request("GET", favoritePath, nil, "", "")
	cleared := decodeResponse[access.AlbumFavorite](t, f.reader.request("DELETE", favoritePath, nil, current.Header().Get("ETag"), store.NewID()), 200)
	if cleared.Favorited || cleared.Version != 3 {
		t.Fatal("clear did not leave a false personal revision", cleared)
	}
	replay := decodeResponse[access.AlbumFavorite](t, f.reader.request("PUT", favoritePath, map[string]any{"collection_id": "selection-http-two"}, otherTag, otherKey), 200)
	if !replay.Favorited || replay.Version != 2 {
		t.Fatal("historical receipt was not returned", replay)
	}
	live := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+continued.Encode(), nil, "", ""), 200)
	if len(live.Items) != 0 || live.HasMore || live.NextCursor != nil {
		t.Fatal("live continuation included an unfavorited album after historical replay", live)
	}
	falsePage = decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"source_id=media-source&tag_id=selection-tag&favorite=false", nil, "", ""), 200)
	if len(falsePage.Items) != 2 || falsePage.Items[0].ID != "selection-other-album" || falsePage.Items[1].ID != "selection-unmapped-album" {
		t.Fatal("clear tombstone and absent state did not both count as false", falsePage)
	}

	if _, err := f.db.Exec(`DELETE FROM user_groups WHERE user_id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	hidden := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+query.Encode(), nil, "", ""), 200)
	if len(hidden.Items) != 0 || hidden.HasMore || hidden.NextCursor != nil {
		t.Fatal("favorite predicate bypassed a fresh library grant", hidden)
	}
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
		for _, state := range []string{"true", "false"} {
			page := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"source_id=media-source&favorite="+state, nil, "", ""), 200)
			if len(page.Items) != 0 {
				t.Fatal("favorite selector bypassed inactive or inconsistent resources", state, page)
			}
		}
		if _, err := f.db.Exec(change.restore); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`UPDATE sources SET state='unavailable' WHERE id='media-source'`); err != nil {
		t.Fatal(err)
	}
	available := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", path+"source_id=media-source&favorite=true", nil, "", ""), 200)
	if len(available.Items) != 1 || available.Items[0].ID != "media-album" {
		t.Fatal("favorite metadata depended on a source mount probe", available)
	}
	if _, err := f.db.Exec(`UPDATE users SET scope_id='favorite-selection-new-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+continued.Encode(), nil, "", ""), 400, "INVALID_CURSOR")
	for _, invalid := range []string{"favorite=", "favorite=TRUE", "favorite=False", "favorite=1", "favorite=0", "favorite=null", "favorite=%20true", "favorite=true&favorite=false", "user_id=other&favorite=true"} {
		expectCode(t, f.reader.request("GET", path+invalid, nil, "", ""), 400, "INVALID_FILTER")
	}
	expectCode(t, (adminClient{handler: f.http}).request("GET", path+"favorite=true", nil, "", ""), 401, "UNAUTHENTICATED")
	for _, secret := range []string{f.source, "root_path", "relative_path", "source_sha256", "selection-http-one", "selection-http-two", "Private assigned label"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("favorite album card disclosed private collection/source metadata", secret)
		}
	}
}
