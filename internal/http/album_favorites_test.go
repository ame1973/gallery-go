package httpapi

import (
	"bytes"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestAlbumFavoritesHTTPPrivateValidatorsAssignmentsReplayAndOutageRetention(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/favorites/media-album"
	initial := f.reader.request("GET", path, nil, "", "")
	before := decodeResponse[access.AlbumFavorite](t, initial, 200)
	adminInitial := f.admin.request("GET", path, nil, "", "")
	decodeResponse[access.AlbumFavorite](t, adminInitial, 200)
	if before.Favorited || before.Version != 1 || before.CollectionID != nil || before.UpdatedAt != nil || initial.Header().Get("ETag") == "" || initial.Header().Get("ETag") == adminInitial.Header().Get("ETag") || initial.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("initial favorite was invented/unversioned/shared/cacheable", before, initial.Header())
	}
	publicPath := "/api/v2/albums/media-album"
	publicBefore := decodeResponse[access.Album](t, f.reader.request("GET", publicPath, nil, "", ""), 200)
	meBefore := decodeResponse[auth.User](t, f.reader.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	key := store.NewID()
	response := f.reader.request("PUT", path, map[string]any{}, initial.Header().Get("ETag"), key)
	changed := decodeResponse[access.AlbumFavorite](t, response, 200)
	if !changed.Favorited || changed.CollectionID != nil || changed.Version != 2 || changed.UpdatedAt == nil || response.Header().Get("ETag") == initial.Header().Get("ETag") {
		t.Fatal("default favorite did not publish a private revision", changed, response.Header())
	}
	adminChanged := f.admin.request("PUT", path, map[string]any{}, adminInitial.Header().Get("ETag"), store.NewID())
	decodeResponse[access.AlbumFavorite](t, adminChanged, 200)
	current := f.reader.request("GET", path, nil, "", "")
	if own := decodeResponse[access.AlbumFavorite](t, current, 200); !reflect.DeepEqual(own, changed) || current.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("another account invalidated a private favorite", own, changed)
	}
	noop := f.reader.request("PUT", path, map[string]any{"collection_id": nil}, response.Header().Get("ETag"), store.NewID())
	if value := decodeResponse[access.AlbumFavorite](t, noop, 200); !reflect.DeepEqual(value, changed) || noop.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("omitted/null default collection did not share exact no-op semantics", value, changed)
	}
	// Isolated metadata rows test assignment ownership, not a collection-creation API.
	if _, err := f.db.Exec(`INSERT INTO favorite_collections(id,user_id,name,created_at) SELECT 'own-favorite-collection',id,'Own',0 FROM users WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorite_collections(id,user_id,name,created_at) SELECT 'foreign-favorite-collection',id,'Own',0 FROM users WHERE role='admin'`); err != nil {
		t.Fatal(err)
	}
	for _, collection := range []string{"foreign-favorite-collection", "missing"} {
		expectCode(t, f.reader.request("PUT", path, map[string]string{"collection_id": collection}, response.Header().Get("ETag"), store.NewID()), 404, "NOT_FOUND")
	}
	movedResponse := f.reader.request("PUT", path, map[string]string{"collection_id": "own-favorite-collection"}, response.Header().Get("ETag"), store.NewID())
	moved := decodeResponse[access.AlbumFavorite](t, movedResponse, 200)
	if !moved.Favorited || moved.Version != 3 || moved.CollectionID == nil || *moved.CollectionID != "own-favorite-collection" {
		t.Fatal("favorite move did not select only an owned exact collection", moved)
	}
	defaultResponse := f.reader.request("PUT", path, map[string]any{"collection_id": nil}, movedResponse.Header().Get("ETag"), store.NewID())
	if value := decodeResponse[access.AlbumFavorite](t, defaultResponse, 200); !value.Favorited || value.CollectionID != nil || value.Version != 4 {
		t.Fatal("explicit default move changed membership or lost its version", value)
	}
	clearedResponse := f.reader.request("DELETE", path, nil, defaultResponse.Header().Get("ETag"), store.NewID())
	cleared := decodeResponse[access.AlbumFavorite](t, clearedResponse, 200)
	if cleared.Favorited || cleared.CollectionID != nil || cleared.Version != 5 || cleared.UpdatedAt == nil {
		t.Fatal("favorite clear did not preserve a false revision", cleared)
	}
	replay := f.reader.request("PUT", path, map[string]any{"collection_id": nil}, initial.Header().Get("ETag"), key)
	if value := decodeResponse[access.AlbumFavorite](t, replay, 200); !reflect.DeepEqual(value, changed) || replay.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("historical favorite receipt was recomputed", value, changed)
	}
	if current := decodeResponse[access.AlbumFavorite](t, f.reader.request("GET", path, nil, "", ""), 200); !reflect.DeepEqual(current, cleared) {
		t.Fatal("historical replay restored an obsolete membership", current, cleared)
	}
	expectCode(t, f.reader.request("PUT", path, map[string]any{}, initial.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	expectCode(t, f.reader.request("PUT", path, map[string]string{"collection_id": "own-favorite-collection"}, initial.Header().Get("ETag"), key), 409, "IDEMPOTENCY_CONFLICT")
	noop = f.reader.request("DELETE", path, nil, clearedResponse.Header().Get("ETag"), store.NewID())
	if value := decodeResponse[access.AlbumFavorite](t, noop, 200); !reflect.DeepEqual(value, cleared) || noop.Header().Get("ETag") != clearedResponse.Header().Get("ETag") {
		t.Fatal("repeated clear rewrote a private tombstone", value)
	}
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	unmounted := decodeResponse[access.AlbumFavorite](t, f.reader.request("PUT", path, map[string]any{}, clearedResponse.Header().Get("ETag"), store.NewID()), 200)
	if !unmounted.Favorited || unmounted.Version != 6 {
		t.Fatal("favorite metadata depended on source reachability", unmounted)
	}
	publicAfter := decodeResponse[access.Album](t, f.reader.request("GET", publicPath, nil, "", ""), 200)
	meAfter := decodeResponse[auth.User](t, f.reader.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	if !reflect.DeepEqual(publicBefore, publicAfter) || !reflect.DeepEqual(meBefore, meAfter) {
		t.Fatal("private favorites changed shared metadata/account/scope", publicBefore, publicAfter, meBefore, meAfter)
	}
	data, err := os.ReadFile(filepath.Join(f.source+"-unmounted", filepath.Base(f.original)))
	if err != nil || !bytes.Equal(data, f.data) {
		t.Fatal("favorite changed owned source bytes", err)
	}
	for _, secret := range []string{f.source, f.cache, "root_path", "source_sha256", "csrf", "password", "user_id"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("favorite response leaked unrelated private state/authority", secret)
		}
	}
}

func TestAlbumFavoritesHTTPStrictInputsFreshAuthorityAccountDeleteAndPrivateFailures(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/favorites/media-album"
	initial := f.reader.request("GET", path, nil, "", "")
	tag := initial.Header().Get("ETag")
	adminInitial := f.admin.request("GET", path, nil, "", "")
	ratingTag := f.reader.request("GET", "/api/v2/albums/media-album/rating", nil, "", "").Header().Get("ETag")
	body := map[string]any{"collection_id": nil}
	guest := adminClient{handler: f.http}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		expectCode(t, guest.request(method, path, body, tag, store.NewID()), 401, "UNAUTHENTICATED")
		expectCode(t, f.reader.request(method, path+"?user_id=other", body, tag, store.NewID()), 400, "INVALID_FILTER")
	}
	expectCode(t, f.reader.request("PUT", path, body, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
	expectCode(t, f.reader.request("PUT", path, body, tag, ""), 422, "VALIDATION_FAILED")
	for _, invalid := range []string{"*", "W/" + tag, tag + "," + tag, adminInitial.Header().Get("ETag"), ratingTag, `"album:media-album:1"`, strings.Replace(tag, ":1\"", ":0\"", 1)} {
		expectCode(t, f.reader.request("PUT", path, body, invalid, store.NewID()), 400, "INVALID_PRECONDITION")
	}
	for _, invalid := range []string{"", " padded", "a/b", `a\b`, "bad\x00", "bad" + string(rune(0x200d)), strings.Repeat("字", 22)} {
		expectCode(t, f.reader.request("PUT", path, map[string]string{"collection_id": invalid}, tag, store.NewID()), 422, "VALIDATION_FAILED")
	}
	for _, raw := range []string{`null`, `[]`, `{"collection_id":true}`, `{"collection_id":5}`, `{"collection_id":null,"collection_id":"x"}`, `{"Collection_ID":null}`, `{"user_id":"other"}`, `{"favorited":true}`, `{"updated_at":"2000-01-01T00:00:00Z"}`, `{} {}`} {
		r := httptest.NewRequest("PUT", path, strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("X-CSRF-Token", f.reader.csrf)
		r.Header.Set("If-Match", tag)
		r.Header.Set("Idempotency-Key", store.NewID())
		r.AddCookie(f.reader.cookie)
		w := httptest.NewRecorder()
		f.http.ServeHTTP(w, r)
		expectCode(t, w, 400, "INVALID_JSON")
	}
	expectCode(t, f.reader.request("DELETE", path, map[string]any{}, tag, store.NewID()), 400, "INVALID_BODY")
	for _, method := range []string{"PUT", "DELETE"} {
		expectCode(t, requestAuth(f.http, method, path, `{}`, testOrigin, "", f.reader.cookie), 403, "CSRF_REJECTED")
	}
	if _, err := f.db.Exec(`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('favorite-parent',?,NULL,'Parent',0),('favorite-child',?,'favorite-parent','Child',0)`, f.user.ID, f.user.ID); err != nil {
		t.Fatal(err)
	}
	key := store.NewID()
	owned := map[string]string{"collection_id": "favorite-child"}
	decodeResponse[access.AlbumFavorite](t, f.reader.request("PUT", path, owned, tag, key), 200)
	if _, err := f.db.Exec(`DELETE FROM user_groups WHERE user_id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.reader.request("PUT", path, owned, tag, key), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES(?,'media-group')`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_http_favorite BEFORE INSERT ON mutation_receipts WHEN NEW.operation='album.favorite.replace' BEGIN SELECT RAISE(ABORT,'/private/SQL/favorite-receipt'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("PUT", path, body, adminInitial.Header().Get("ETag"), store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	for _, secret := range []string{"/private", "SQL", "favorite-receipt", f.source} {
		if strings.Contains(problemContent(t, failed), secret) {
			t.Fatal("favorite failure leaked private implementation details", failed.Body.String())
		}
	}
	if value := decodeResponse[access.AlbumFavorite](t, f.admin.request("GET", path, nil, "", ""), 200); value.Favorited || value.Version != 1 {
		t.Fatal("failed receipt retained a partial favorite", value)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_http_favorite`); err != nil {
		t.Fatal(err)
	}
	adminKey := store.NewID()
	adminResponse := f.admin.request("PUT", path, body, adminInitial.Header().Get("ETag"), adminKey)
	adminFavorite := decodeResponse[access.AlbumFavorite](t, adminResponse, 200)
	if !adminFavorite.Favorited || adminFavorite.Version != 2 || adminFavorite.UpdatedAt == nil {
		t.Fatal("administrator did not get an independent personal membership", adminFavorite)
	}
	if _, err := f.db.Exec(`UPDATE favorites SET updated_at=? WHERE user_id IN (SELECT id FROM users WHERE role='admin')`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	invalid := f.admin.request("GET", path, nil, "", "")
	expectCode(t, invalid, 500, "INTERNAL_ERROR")
	if invalid.Header().Get("ETag") != "" || strings.Contains(problemContent(t, invalid), "stored favorite") || strings.Contains(problemContent(t, invalid), "UTC range") {
		t.Fatal("invalid favorite instant emitted a partial success/private detail", invalid.Header(), invalid.Body.String())
	}
	if _, err := f.db.Exec(`UPDATE favorites SET updated_at=? WHERE user_id IN (SELECT id FROM users WHERE role='admin')`, adminFavorite.UpdatedAt.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	publicBefore := decodeResponse[access.Album](t, f.admin.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	userPath := "/api/v2/admin/users/" + f.user.ID
	userResponse := f.admin.request("GET", userPath, nil, "", "")
	decodeResponse[auth.User](t, userResponse, 200)
	if deleted := f.admin.request("DELETE", userPath, nil, userResponse.Header().Get("ETag"), ""); deleted.Code != 204 {
		t.Fatal("real account deletion could not cascade its nested favorite collections", deleted.Code, deleted.Body.String())
	}
	expectCode(t, f.reader.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
	var remaining int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorites WHERE user_id=?)+(SELECT COUNT(*) FROM favorite_collections WHERE user_id=?)`, f.user.ID, f.user.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("account removal retained private favorite data", remaining, err)
	}
	if current := decodeResponse[access.AlbumFavorite](t, f.admin.request("GET", path, nil, "", ""), 200); !reflect.DeepEqual(current, adminFavorite) {
		t.Fatal("account removal changed another account's favorite", current, adminFavorite)
	}
	if public := decodeResponse[access.Album](t, f.admin.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200); !reflect.DeepEqual(public, publicBefore) {
		t.Fatal("private favorite account removal changed shared gallery metadata", public, publicBefore)
	}
	if _, err := f.db.Exec(`UPDATE sources SET is_active=0 WHERE id='media-source'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.admin.request("PUT", path, body, adminInitial.Header().Get("ETag"), adminKey), 404, "NOT_FOUND")
}
