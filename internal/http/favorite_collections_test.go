package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestFavoriteCollectionsHTTPCreateRenameOwnedPagesAndFreshAuthority(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/favorite-collections"
	guest := adminClient{handler: f.http}
	expectCode(t, guest.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
	expectCode(t, guest.request("POST", path, map[string]string{"name": "Root"}, "", store.NewID()), 401, "UNAUTHENTICATED")
	createdKey := store.NewID()
	createdResponse := f.reader.request("POST", path, map[string]string{"name": "  Root  "}, "", createdKey)
	root := decodeResponse[access.FavoriteCollection](t, createdResponse, 201)
	rootPath := path + "/" + root.ID
	if root.Name != "Root" || root.ParentID != nil || root.Version != 1 || !root.CreatedAt.Equal(root.UpdatedAt) || createdResponse.Header().Get("Location") != rootPath || createdResponse.Header().Get("ETag") == "" || createdResponse.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("root response lost private version, time or Location", root, createdResponse.Header())
	}
	replay := f.reader.request("POST", path, map[string]any{"name": "Root", "parent_id": nil}, "", createdKey)
	if got := decodeResponse[access.FavoriteCollection](t, replay, 201); !reflect.DeepEqual(got, root) || replay.Header().Get("ETag") != createdResponse.Header().Get("ETag") {
		t.Fatal("canonical root retry was not stable", got, root)
	}
	expectCode(t, f.reader.request("POST", path, map[string]string{"name": "Different"}, "", createdKey), 409, "IDEMPOTENCY_CONFLICT")
	expectCode(t, f.reader.request("POST", path, map[string]string{"name": "Root"}, "", store.NewID()), 409, "COLLECTION_NAME_TAKEN")
	adminRootResponse := f.admin.request("POST", path, map[string]string{"name": "Root"}, "", store.NewID())
	adminRoot := decodeResponse[access.FavoriteCollection](t, adminRootResponse, 201)
	expectCode(t, f.admin.request("GET", rootPath, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.reader.request("GET", path+"/"+adminRoot.ID, nil, "", ""), 404, "NOT_FOUND")
	for _, id := range []string{adminRoot.ID, "missing"} {
		expectCode(t, f.reader.request("POST", path, map[string]string{"name": "Foreign", "parent_id": id}, "", store.NewID()), 404, "NOT_FOUND")
		expectCode(t, f.reader.request("GET", path+"?parent_id="+url.QueryEscape(id), nil, "", ""), 404, "NOT_FOUND")
	}
	childResponse := f.reader.request("POST", path, map[string]string{"name": "Child", "parent_id": root.ID}, "", store.NewID())
	child := decodeResponse[access.FavoriteCollection](t, childResponse, 201)
	if child.ParentID == nil || *child.ParentID != root.ID {
		t.Fatal("child lost its owned parent", child)
	}
	childTwo := decodeResponse[access.FavoriteCollection](t, f.reader.request("POST", path, map[string]string{"name": "Another", "parent_id": root.ID}, "", store.NewID()), 201)
	second := decodeResponse[access.FavoriteCollection](t, f.reader.request("POST", path, map[string]string{"name": "Second"}, "", store.NewID()), 201)
	if second.ParentID != nil {
		t.Fatal("omitted parent did not select a root", second)
	}
	all := decodeResponse[listPage[access.FavoriteCollection]](t, f.reader.request("GET", path, nil, "", ""), 200)
	if len(all.Items) != 4 || all.HasMore || all.NextCursor != nil {
		t.Fatal("flat list lost or leaked collections", all)
	}
	children := decodeResponse[listPage[access.FavoriteCollection]](t, f.reader.request("GET", path+"?parent_id="+url.QueryEscape(root.ID), nil, "", ""), 200)
	if len(children.Items) != 2 || children.HasMore || children.NextCursor != nil {
		t.Fatal("child list lost its owned parent intersection", children)
	}
	ids := []string{children.Items[0].ID, children.Items[1].ID}
	want := []string{child.ID, childTwo.ID}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Fatal("child page order or identity changed", ids, want)
	}
	first := decodeResponse[listPage[access.FavoriteCollection]](t, f.reader.request("GET", path+"?root=true&limit=1", nil, "", ""), 200)
	if len(first.Items) != 1 || !first.HasMore || first.NextCursor == nil {
		t.Fatal("root page did not expose a bounded continuation", first)
	}
	continued := "root=true&limit=100&cursor=" + url.QueryEscape(*first.NextCursor)
	last := decodeResponse[listPage[access.FavoriteCollection]](t, f.reader.request("GET", path+"?"+continued, nil, "", ""), 200)
	if len(last.Items) != 1 || last.HasMore || last.NextCursor != nil || first.Items[0].ID >= last.Items[0].ID {
		t.Fatal("root keysets repeated or lost a folder", first, last)
	}
	expectCode(t, f.reader.request("GET", path+"?limit=100&cursor="+url.QueryEscape(*first.NextCursor), nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, f.admin.request("GET", path+"?"+continued, nil, "", ""), 400, "INVALID_CURSOR")
	data, err := base64.RawURLEncoding.DecodeString(*first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	var cursor pageCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		t.Fatal(err)
	}
	cursor.Position = "not-a-folder-key"
	data, err = json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?root=true&cursor="+base64.RawURLEncoding.EncodeToString(data), nil, "", ""), 400, "INVALID_CURSOR")

	for _, invalid := range []string{"root=", "root=false", "root=TRUE", "root=1", "root=true&root=true", "parent_id=", "parent_id=a%2Fb", "root=true&parent_id=" + root.ID, "user_id=other", "Parent_ID=" + root.ID} {
		expectCode(t, f.reader.request("GET", path+"?"+invalid, nil, "", ""), 400, "INVALID_FILTER")
	}
	expectCode(t, f.reader.request("GET", path+"?cursor=", nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, f.reader.request("GET", rootPath+"?user_id=other", nil, "", ""), 400, "INVALID_FILTER")
	expectCode(t, f.reader.request("POST", path+"?user_id=other", map[string]string{"name": "No"}, "", store.NewID()), 400, "INVALID_FILTER")
	expectCode(t, f.reader.request("PATCH", rootPath, map[string]string{"name": "Rename"}, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
	expectCode(t, f.reader.request("PATCH", rootPath, map[string]string{"name": "Rename"}, adminRootResponse.Header().Get("ETag"), store.NewID()), 400, "INVALID_PRECONDITION")
	expectCode(t, f.reader.request("PATCH", rootPath, map[string]string{"name": "Rename"}, createdResponse.Header().Get("ETag"), ""), 422, "VALIDATION_FAILED")
	for _, raw := range []string{`{"name":null}`, `{"name":"x","parent_id":null}`, `{"name":"x","user_id":"other"}`, `{"name":"x","version":1}`, `{"Name":"x"}`, `{"name":"x","name":"y"}`} {
		r := httptest.NewRequest("PATCH", rootPath, strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("X-CSRF-Token", f.reader.csrf)
		r.Header.Set("If-Match", createdResponse.Header().Get("ETag"))
		r.Header.Set("Idempotency-Key", store.NewID())
		r.AddCookie(f.reader.cookie)
		w := httptest.NewRecorder()
		f.http.ServeHTTP(w, r)
		expectCode(t, w, 400, "INVALID_JSON")
	}
	expectCode(t, f.reader.request("PATCH", rootPath, map[string]any{}, createdResponse.Header().Get("ETag"), store.NewID()), 422, "VALIDATION_FAILED")

	favoritePath := "/api/v2/favorites/media-album"
	favoriteInitial := f.reader.request("GET", favoritePath, nil, "", "")
	publicBefore := decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	meBefore := decodeResponse[auth.User](t, f.reader.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	assigned := decodeResponse[access.AlbumFavorite](t, f.reader.request("PUT", favoritePath, map[string]string{"collection_id": child.ID}, favoriteInitial.Header().Get("ETag"), store.NewID()), 200)
	if !assigned.Favorited || assigned.CollectionID == nil || *assigned.CollectionID != child.ID {
		t.Fatal("new collection could not be assigned to a real favorite", assigned)
	}
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	renameKey := store.NewID()
	renamedResponse := f.reader.request("PATCH", rootPath, map[string]string{"name": "Renamed"}, createdResponse.Header().Get("ETag"), renameKey)
	renamed := decodeResponse[access.FavoriteCollection](t, renamedResponse, 200)
	if renamed.Name != "Renamed" || renamed.Version != 2 || renamed.UpdatedAt.Before(root.UpdatedAt) {
		t.Fatal("rename lost personal revision/time", renamed)
	}
	latestResponse := f.reader.request("PATCH", rootPath, map[string]string{"name": "Latest"}, renamedResponse.Header().Get("ETag"), store.NewID())
	latest := decodeResponse[access.FavoriteCollection](t, latestResponse, 200)
	if latest.Version != 3 {
		t.Fatal("second rename did not advance version", latest)
	}
	replayed := decodeResponse[access.FavoriteCollection](t, f.reader.request("PATCH", rootPath, map[string]string{"name": "Renamed"}, createdResponse.Header().Get("ETag"), renameKey), 200)
	if !reflect.DeepEqual(replayed, renamed) || !reflect.DeepEqual(decodeResponse[access.FavoriteCollection](t, f.reader.request("GET", rootPath, nil, "", ""), 200), latest) {
		t.Fatal("historical rename replay restored an obsolete name", replayed, renamed, latest)
	}
	expectCode(t, f.reader.request("PATCH", rootPath, map[string]string{"name": "Renamed"}, createdResponse.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	if got := decodeResponse[access.AlbumFavorite](t, f.reader.request("GET", favoritePath, nil, "", ""), 200); !reflect.DeepEqual(got, assigned) {
		t.Fatal("collection metadata changed a real favorite", got, assigned)
	}
	if got := decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200); !reflect.DeepEqual(got, publicBefore) {
		t.Fatal("collection metadata changed a shared album", got, publicBefore)
	}
	if got := decodeResponse[auth.User](t, f.reader.request("GET", "/api/v2/auth/me", nil, "", ""), 200); !reflect.DeepEqual(got, meBefore) {
		t.Fatal("collection metadata rotated or changed account authority", got, meBefore)
	}
	for _, secret := range []string{"user_id", "album_id", "media-album", "root_path", "relative_path", f.source} {
		if strings.Contains(createdResponse.Body.String()+renamedResponse.Body.String(), secret) {
			t.Fatal("collection metadata exposed unrelated private content", secret)
		}
	}

	if _, err := f.db.Exec(`UPDATE users SET scope_id='collection-new-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?"+continued, nil, "", ""), 400, "INVALID_CURSOR")
	userPath := "/api/v2/admin/users/" + f.user.ID
	currentUser := f.admin.request("GET", userPath, nil, "", "")
	if deleted := f.admin.request("DELETE", userPath, nil, currentUser.Header().Get("ETag"), ""); deleted.Code != 204 {
		t.Fatal("real account deletion could not cascade created nested collections", deleted.Code, deleted.Body.String())
	}
	expectCode(t, f.reader.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
	if got := decodeResponse[access.FavoriteCollection](t, f.admin.request("GET", path+"/"+adminRoot.ID, nil, "", ""), 200); !reflect.DeepEqual(got, adminRoot) {
		t.Fatal("account deletion changed another owner's collection", got, adminRoot)
	}
	var remaining int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE user_id=?)+(SELECT COUNT(*) FROM favorites WHERE user_id=?)`, f.user.ID, f.user.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("account deletion retained owned collections or favorites", remaining, err)
	}
}

func TestFavoriteCollectionsHTTPRejectStrictBodiesAndRollbackLateFailures(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/favorite-collections"
	for _, raw := range []string{`null`, `[]`, `{"name":null}`, `{"name":5}`, `{"name":"x","parent_id":5}`, `{"name":"x","parent_id":null,"parent_id":"x"}`, `{"Name":"x"}`, `{"name":"x","user_id":"other"}`, `{"name":"x","version":1}`, `{} {}`} {
		r := httptest.NewRequest("POST", path, strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("X-CSRF-Token", f.reader.csrf)
		r.Header.Set("Idempotency-Key", store.NewID())
		r.AddCookie(f.reader.cookie)
		w := httptest.NewRecorder()
		f.http.ServeHTTP(w, r)
		expectCode(t, w, 400, "INVALID_JSON")
	}
	expectCode(t, f.reader.request("POST", path, map[string]any{"name": "x", "parent_id": ""}, "", store.NewID()), 422, "VALIDATION_FAILED")
	expectCode(t, f.reader.request("POST", path, map[string]any{"name": "x", "parent_id": "/private"}, "", store.NewID()), 422, "VALIDATION_FAILED")
	expectCode(t, f.reader.request("POST", path, map[string]string{"name": " "}, "", store.NewID()), 422, "VALIDATION_FAILED")
	expectCode(t, requestAuth(f.http, "POST", path, `{"name":"x"}`, testOrigin, "", f.reader.cookie), 403, "CSRF_REJECTED")
	if _, err := f.db.Exec(`CREATE TRIGGER fail_collection_receipt BEFORE INSERT ON mutation_receipts WHEN NEW.operation='favorite_collection.create' BEGIN SELECT RAISE(ABORT,'/private/SQL/collection-receipt'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.reader.request("POST", path, map[string]string{"name": "Rollback"}, "", store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	for _, secret := range []string{"/private", "SQL", "collection-receipt", f.source} {
		if strings.Contains(problemContent(t, failed), secret) {
			t.Fatal("collection failure leaked internal detail", secret)
		}
	}
	var rows, events, audit, receipts int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE user_id=?),
 (SELECT COUNT(*) FROM change_log WHERE kind='favorite_collection'),
 (SELECT COUNT(*) FROM audit_events WHERE event_type='favorite_collection_created'),
 (SELECT COUNT(*) FROM mutation_receipts WHERE operation='favorite_collection.create')`, f.user.ID).Scan(&rows, &events, &audit, &receipts); err != nil || rows != 0 || events != 0 || audit != 0 || receipts != 0 {
		t.Fatal("failed collection create left partial state", rows, events, audit, receipts, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_collection_receipt`); err != nil {
		t.Fatal(err)
	}
	created := decodeResponse[access.FavoriteCollection](t, f.reader.request("POST", path, map[string]string{"name": "Valid"}, "", store.NewID()), 201)
	if _, err := f.db.Exec(`UPDATE favorite_collections SET updated_at=9223372036854775807 WHERE id=?`, created.ID); err != nil {
		t.Fatal(err)
	}
	invalid := f.reader.request("GET", path+"/"+created.ID, nil, "", "")
	expectCode(t, invalid, 500, "INTERNAL_ERROR")
	if invalid.Header().Get("ETag") != "" || strings.Contains(problemContent(t, invalid), "stored collection") {
		t.Fatal("invalid stored timestamp leaked a validator or private diagnostic", invalid.Header(), invalid.Body.String())
	}
}
