package httpapi

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func albumTagsHTTPFixture(t *testing.T) protectedFixture {
	t.Helper()
	f := protectedMedia(t)
	if _, err := f.db.Exec(`INSERT INTO tags(id,name) VALUES('tag-one','Visible first'),('tag-two','Visible second'),('tag-hidden','Hidden private label');
 INSERT INTO albums(id,library_id,source_id,source_name,access_restricted,created_at,updated_at) VALUES('other-tag-album','media-library','media-source','Hidden tags',1,0,0);
 INSERT INTO album_tags(album_id,tag_id) VALUES('other-tag-album','tag-hidden')`); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAlbumTagsHTTPVersionsHistoricalReceiptsAndLabelSafeProjection(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	const path = "/api/v2/admin/albums/media-album/tags"
	const publicPath = "/api/v2/albums/media-album/tags"
	initial := f.admin.request("GET", path, nil, "", "")
	before := decodeResponse[access.AlbumTagSet](t, initial, 200)
	if before.Version != 1 || before.TagIDs == nil || len(before.TagIDs) != 0 || initial.Header().Get("ETag") != `"album:media-album:1"` {
		t.Fatal("empty association state lacked exact IDs/version", before, initial.Header())
	}
	var scope string
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.user.ID).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	key := store.NewID()
	body := map[string]any{"tag_ids": []string{"tag-two", "tag-one"}}
	response := f.admin.request("PUT", path, body, initial.Header().Get("ETag"), key)
	changed := decodeResponse[access.AlbumTagSet](t, response, 200)
	if changed.Version != 2 || !slices.Equal(changed.TagIDs, []string{"tag-one", "tag-two"}) || response.Header().Get("ETag") != `"album:media-album:2"` {
		t.Fatal("tag replacement did not publish sorted IDs and shared album version", changed, response.Header())
	}
	publicResponse := f.reader.request("GET", publicPath, nil, "", "")
	public := decodeResponse[listPage[access.Tag]](t, publicResponse, 200)
	if len(public.Items) != 2 || public.Items[0].ID != "tag-one" || public.Items[1].ID != "tag-two" || publicResponse.Header().Get("ETag") != "" || publicResponse.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("label-bearing public page leaked labels or claimed an album-only ETag", public, publicResponse.Header())
	}
	renamed := decodeResponse[access.Tag](t, f.admin.request("PATCH", "/api/v2/admin/tags/tag-one", map[string]string{"name": "Changed label"}, `"tag:tag-one:1"`, store.NewID()), 200)
	if renamed.Version != 2 {
		t.Fatal("tag rename fixture failed", renamed)
	}
	still := f.admin.request("GET", path, nil, "", "")
	if current := decodeResponse[access.AlbumTagSet](t, still, 200); !reflect.DeepEqual(current, changed) || still.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("separate tag label change invalidated an IDs-only association ETag", current, changed)
	}
	public = decodeResponse[listPage[access.Tag]](t, f.reader.request("GET", publicPath, nil, "", ""), 200)
	if public.Items[0].Name != "Changed label" || public.Items[0].Version != 2 {
		t.Fatal("public association names did not use current tag metadata", public)
	}
	noop := decodeResponse[access.AlbumTagSet](t, f.admin.request("PUT", path, body, response.Header().Get("ETag"), store.NewID()), 200)
	if !reflect.DeepEqual(noop, changed) {
		t.Fatal("identical tag set was not a version no-op", noop, changed)
	}
	clearResponse := f.admin.request("PUT", path, map[string]any{"tag_ids": []string{}}, response.Header().Get("ETag"), store.NewID())
	cleared := decodeResponse[access.AlbumTagSet](t, clearResponse, 200)
	if cleared.Version != 3 || cleared.TagIDs == nil || len(cleared.TagIDs) != 0 {
		t.Fatal("clear did not preserve explicit empty association metadata", cleared)
	}
	public = decodeResponse[listPage[access.Tag]](t, f.reader.request("GET", publicPath, nil, "", ""), 200)
	if public.Items == nil || len(public.Items) != 0 || public.HasMore || public.NextCursor != nil {
		t.Fatal("cleared associations leaked unused/private catalog tags", public)
	}
	replayed := f.admin.request("PUT", path, body, initial.Header().Get("ETag"), key)
	if value := decodeResponse[access.AlbumTagSet](t, replayed, 200); !reflect.DeepEqual(value, changed) || replayed.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("historical association result was retargeted by clear", value, changed)
	}
	expectCode(t, f.admin.request("PUT", path, body, initial.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	unmounted := decodeResponse[access.AlbumTagSet](t, f.admin.request("PUT", path, map[string]any{"tag_ids": []string{"tag-one"}}, clearResponse.Header().Get("ETag"), store.NewID()), 200)
	if unmounted.Version != 4 || !slices.Equal(unmounted.TagIDs, []string{"tag-one"}) {
		t.Fatal("unavailable mount prevented indexed tag association management", unmounted)
	}
	var currentScope string
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.user.ID).Scan(&currentScope); err != nil || currentScope != scope {
		t.Fatal("tag association edit changed account authorization scope", scope, currentScope, err)
	}
	for _, secret := range []string{f.source, "Hidden private label", "root_path", "relative_path", "source_config"} {
		if strings.Contains(publicResponse.Body.String()+response.Body.String(), secret) {
			t.Fatal("album tag projection disclosed unrelated/private filesystem metadata", secret)
		}
	}
}

func TestAlbumTagsHTTPBoundedPagesHiddenAlbumsAndScopedCursors(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 100 {
		id := fmt.Sprintf("page-tag-%03d", index)
		if _, err := tx.Exec(`INSERT INTO tags(id,name) VALUES(?,?)`, id, "Label "+id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO album_tags(album_id,tag_id) VALUES('media-album',?)`, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v2/albums/media-album/tags"
	first := decodeResponse[listPage[access.Tag]](t, f.reader.request("GET", path, nil, "", ""), 200)
	if len(first.Items) != 30 || !first.HasMore || first.NextCursor == nil {
		t.Fatal("album tag page did not use bounded default keysets", first)
	}
	last := decodeResponse[listPage[access.Tag]](t, f.reader.request("GET", path+"?limit=100&cursor="+*first.NextCursor, nil, "", ""), 200)
	if len(last.Items) != 70 || last.HasMore || last.NextCursor != nil || last.Items[69].ID != "page-tag-099" {
		t.Fatal("album tag cursor lost or duplicated authorized labels", last)
	}
	for _, route := range []string{"/api/v2/albums/other-tag-album/tags", "/api/v2/albums/missing/tags"} {
		expectCode(t, f.reader.request("GET", route, nil, "", ""), 404, "NOT_FOUND")
	}
	for _, route := range []string{"/api/v2/albums/other-tag-album/tags", "/api/v2/albums/media-album/media"} {
		expectCode(t, f.reader.request("GET", route+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2"} {
		expectCode(t, f.reader.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	expectCode(t, f.reader.request("GET", path+"?name=ignored", nil, "", ""), 400, "INVALID_FILTER")
	if _, err := f.db.Exec(`UPDATE users SET scope_id='new-album-tag-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	if _, err := f.db.Exec(`UPDATE sources SET is_active=0 WHERE id='media-source'`); err != nil {
		t.Fatal(err)
	}
	for _, client := range []adminClient{f.admin, f.reader} {
		expectCode(t, client.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	}
}

func TestAlbumTagsHTTPStrictCommandsRolesAndLateRollback(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	const path = "/api/v2/admin/albums/media-album/tags"
	tag := `"album:media-album:1"`
	guest := adminClient{handler: f.http}
	expectCode(t, guest.request("GET", "/api/v2/albums/media-album/tags", nil, "", ""), 401, "UNAUTHENTICATED")
	for _, route := range []string{path, "/api/v2/admin/albums/missing/tags"} {
		expectCode(t, guest.request("GET", route, nil, "", ""), 401, "UNAUTHENTICATED")
		expectCode(t, f.reader.request("GET", route, nil, "", ""), 403, "ADMIN_REQUIRED")
		expectCode(t, f.reader.request("PUT", route, map[string]any{"tag_ids": []string{}}, tag, store.NewID()), 403, "ADMIN_REQUIRED")
	}
	for _, body := range []map[string]any{{"tag_ids": nil}, {"tag_ids": "tag-one"}, {"Tag_IDs": []string{}}, {"restricted": false}, {"source_id": "other"}, {"version": 7}} {
		expectCode(t, f.admin.request("PUT", path, body, tag, store.NewID()), 400, "INVALID_JSON")
	}
	for _, body := range []map[string]any{{}, {"tag_ids": []string{""}}, {"tag_ids": []string{"tag-one", "tag-one"}}, {"tag_ids": []string{"missing"}}, {"tag_ids": make([]string, 101)}} {
		expectCode(t, f.admin.request("PUT", path, body, tag, store.NewID()), 422, "VALIDATION_FAILED")
	}
	expectCode(t, f.admin.request("PUT", path, map[string]any{"tag_ids": []string{}}, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
	expectCode(t, f.admin.request("PUT", path, map[string]any{"tag_ids": []string{}}, tag, ""), 422, "VALIDATION_FAILED")
	for _, invalid := range []string{"*", "W/" + tag, `"album:other:1"`, `"tag:media-album:1"`} {
		expectCode(t, f.admin.request("PUT", path, map[string]any{"tag_ids": []string{}}, invalid, store.NewID()), 400, "INVALID_PRECONDITION")
	}
	expectCode(t, requestAuth(f.http, "PUT", path, `{"tag_ids":[]}`, testOrigin, "", f.admin.cookie), 403, "CSRF_REJECTED")
	if _, err := f.db.Exec(`CREATE TRIGGER reject_album_tags_receipt BEFORE INSERT ON mutation_receipts BEGIN SELECT RAISE(ABORT,'/private/SQL/receipt'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("PUT", path, map[string]any{"tag_ids": []string{"tag-one"}}, tag, store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(problemContent(t, failed), "/private") || strings.Contains(problemContent(t, failed), "SQL") || strings.Contains(problemContent(t, failed), "receipt") {
		t.Fatal("late association failure exposed private DB context", failed.Body.String())
	}
	current := decodeResponse[access.AlbumTagSet](t, f.admin.request("GET", path, nil, "", ""), 200)
	if current.Version != 1 || current.TagIDs == nil || len(current.TagIDs) != 0 {
		t.Fatal("failed association receipt left rows or version changes", current)
	}
}
