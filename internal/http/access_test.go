package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func decodeResponse[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	if w.Code != status {
		t.Fatalf("response=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
	var value T
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func createHTTPGroup(t *testing.T, client adminClient, name string) access.Group {
	t.Helper()
	w := client.request("POST", "/api/v2/admin/groups", map[string]string{"name": name}, "", store.NewID())
	group := decodeResponse[access.Group](t, w, 201)
	if w.Header().Get("ETag") != resourceETag("group", group.ID, group.Version) || w.Header().Get("Location") != "/api/v2/admin/groups/"+group.ID {
		t.Fatal("missing group version or location")
	}
	return group
}

func createHTTPLibrary(t *testing.T, client adminClient, name string) access.Library {
	t.Helper()
	w := client.request("POST", "/api/v2/admin/libraries", map[string]string{"name": name}, "", store.NewID())
	library := decodeResponse[access.Library](t, w, 201)
	if !library.Active || w.Header().Get("ETag") != resourceETag("library", library.ID, library.Version) || w.Header().Get("Location") != "/api/v2/admin/libraries/"+library.ID {
		t.Fatal("missing library defaults, version, or location")
	}
	return library
}

func seedHTTPAlbum(t *testing.T, db *sql.DB, libraryID, title string) string {
	t.Helper()
	id := store.NewID()
	if _, err := db.Exec(`INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES(?,?,?,0,0)`, id, libraryID, title); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAccessHTTPMandatoryLibraryAdditionalAlbumAndDeletionLatch(t *testing.T) {
	db, h := authFixture(t)
	admin, _ := ownerClient(t, h)
	user := createHTTPUser(t, admin, "reader", "user")
	viewer := accountClient(t, h, user.Username, testPassword)
	base, restricted, library := createHTTPGroup(t, admin, "Library"), createHTTPGroup(t, admin, "Restricted"), createHTTPLibrary(t, admin, "Library")
	open, limited := seedHTTPAlbum(t, db, library.ID, "Open"), seedHTTPAlbum(t, db, library.ID, "Private")
	membershipPath := "/api/v2/admin/users/" + user.ID + "/groups"
	w := admin.request("PUT", membershipPath, map[string]any{"group_ids": []string{base.ID, restricted.ID}}, userETag(user), "")
	membership := decodeResponse[access.UserGroups](t, w, 200)
	if membership.Version != 2 || membership.ScopeID == user.ScopeID || len(membership.GroupIDs) != 2 || w.Header().Get("ETag") != resourceETag("user", user.ID, membership.Version) {
		t.Fatal("membership did not rotate scope/version", membership)
	}
	albumPolicyPath := "/api/v2/admin/albums/" + limited + "/access"
	decodeResponse[access.AlbumPolicy](t, admin.request("PUT", albumPolicyPath, map[string]any{"restricted": true, "group_ids": []string{restricted.ID}}, resourceETag("album", limited, 1), ""), 200)
	expectCode(t, viewer.request("GET", "/api/v2/albums/"+limited, nil, "", ""), 404, "NOT_FOUND")
	page := decodeResponse[listPage[access.Album]](t, viewer.request("GET", "/api/v2/albums", nil, "", ""), 200)
	if page.Items == nil || len(page.Items) != 0 || page.HasMore || page.NextCursor != nil {
		t.Fatal("album group bypassed the required library grant", page)
	}
	libraryPolicyPath := "/api/v2/admin/libraries/" + library.ID + "/access"
	decodeResponse[access.LibraryPolicy](t, admin.request("PUT", libraryPolicyPath, map[string]any{"group_ids": []string{base.ID}}, resourceETag("library", library.ID, 1), ""), 200)
	page = decodeResponse[listPage[access.Album]](t, viewer.request("GET", "/api/v2/albums?limit=1", nil, "", ""), 200)
	if len(page.Items) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatal("authorized keyset page missing", page)
	}
	oldCursor := *page.NextCursor
	second := decodeResponse[listPage[access.Album]](t, viewer.request("GET", "/api/v2/albums?limit=1&cursor="+oldCursor, nil, "", ""), 200)
	if len(second.Items) != 1 || second.Items[0].ID == page.Items[0].ID || second.HasMore {
		t.Fatal("album cursor repeated or lost a row", second)
	}
	for _, id := range []string{open, limited} {
		album := decodeResponse[access.Album](t, viewer.request("GET", "/api/v2/albums/"+id, nil, "", ""), 200)
		if album.MediaCount != 0 || album.CoverMediaID != nil || album.RatingAvg != nil {
			t.Fatal("empty fixture has invented aggregates", album)
		}
	}
	libraries := decodeResponse[listPage[access.Library]](t, viewer.request("GET", "/api/v2/libraries", nil, "", ""), 200)
	if len(libraries.Items) != 1 || libraries.Items[0].ID != library.ID {
		t.Fatal("library visibility mismatch", libraries)
	}
	if w := admin.request("DELETE", "/api/v2/admin/groups/"+restricted.ID, nil, resourceETag("group", restricted.ID, 1), ""); w.Code != 204 {
		t.Fatal("group delete failed", w.Body.String())
	}
	policy := decodeResponse[access.AlbumPolicy](t, admin.request("GET", albumPolicyPath, nil, "", ""), 200)
	if !policy.Restricted || len(policy.GroupIDs) != 0 || policy.Version != 3 {
		t.Fatal("deleting the last group widened access", policy)
	}
	expectCode(t, viewer.request("GET", "/api/v2/albums/"+limited, nil, "", ""), 404, "NOT_FOUND")
	decodeResponse[access.Album](t, viewer.request("GET", "/api/v2/albums/"+open, nil, "", ""), 200)
	expectCode(t, viewer.request("GET", "/api/v2/albums?cursor="+oldCursor, nil, "", ""), 400, "INVALID_CURSOR")
	if w := viewer.request("GET", "/api/v2/auth/me", nil, "", ""); w.Code != 200 {
		t.Fatal("grant changes should preserve a valid login session")
	}
	decodeResponse[access.AlbumPolicy](t, admin.request("PUT", albumPolicyPath, map[string]any{"restricted": false, "group_ids": []string{}}, resourceETag("album", limited, policy.Version), ""), 200)
	decodeResponse[access.Album](t, viewer.request("GET", "/api/v2/albums/"+limited, nil, "", ""), 200)
	current := admin.request("GET", libraryPolicyPath, nil, "", "")
	decodeResponse[access.LibraryPolicy](t, admin.request("PUT", libraryPolicyPath, map[string]any{"group_ids": []string{}}, current.Header().Get("ETag"), ""), 200)
	expectCode(t, viewer.request("GET", "/api/v2/libraries/"+library.ID, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, viewer.request("GET", "/api/v2/albums/"+open, nil, "", ""), 404, "NOT_FOUND")
}

func TestAccessHTTPStrictPolicyBodiesStrongTagsAndIdempotency(t *testing.T) {
	db, h := authFixture(t)
	admin, _ := ownerClient(t, h)
	g, lib := createHTTPGroup(t, admin, "Group"), createHTTPLibrary(t, admin, "Library")
	id := seedHTTPAlbum(t, db, lib.ID, "Album")
	path := "/api/v2/admin/albums/" + id + "/access"
	tag := resourceETag("album", id, 1)
	for _, body := range []any{
		map[string]any{"restricted": nil, "group_ids": []string{}},
		map[string]any{"restricted": true, "group_ids": nil},
		map[string]any{"restricted": true, "group_ids": "all"},
		map[string]any{"restricted": true, "group_ids": []string{}, "scope_id": "forged"},
	} {
		expectCode(t, admin.request("PUT", path, body, tag, ""), 400, "INVALID_JSON")
	}
	for _, body := range []any{
		map[string]any{"group_ids": []string{}}, map[string]any{"restricted": true},
		map[string]any{"restricted": true, "group_ids": []string{g.ID, g.ID}},
		map[string]any{"restricted": true, "group_ids": []string{"unknown"}},
		map[string]any{"restricted": false, "group_ids": []string{g.ID}},
	} {
		expectCode(t, admin.request("PUT", path, body, tag, ""), 422, "VALIDATION_FAILED")
	}
	body := map[string]any{"restricted": true, "group_ids": []string{g.ID}}
	expectCode(t, admin.request("PUT", path, body, "", ""), 428, "PRECONDITION_REQUIRED")
	for _, invalidTag := range []string{"*", "W/" + tag, resourceETag("group", id, 1), resourceETag("album", "wrong", 1), tag + ", " + tag} {
		expectCode(t, admin.request("PUT", path, body, invalidTag, ""), 400, "INVALID_PRECONDITION")
	}
	expectCode(t, admin.request("PUT", path, body, resourceETag("album", id, 9), ""), 409, "VERSION_CONFLICT")
	key := store.NewID()
	input := map[string]string{"name": "Idempotent"}
	created := decodeResponse[access.Group](t, admin.request("POST", "/api/v2/admin/groups", input, "", key), 201)
	replay := decodeResponse[access.Group](t, admin.request("POST", "/api/v2/admin/groups", input, "", key), 201)
	if created != replay {
		t.Fatal("group POST repeated mutation")
	}
	expectCode(t, admin.request("POST", "/api/v2/admin/libraries", input, "", key), 409, "IDEMPOTENCY_CONFLICT")
	expectCode(t, admin.request("POST", "/api/v2/admin/groups", input, "", store.NewID()), 409, "GROUP_NAME_TAKEN")
	expectCode(t, admin.request("POST", "/api/v2/admin/groups", input, "", ""), 422, "VALIDATION_FAILED")
	expectCode(t, admin.request("POST", "/api/v2/admin/libraries", input, "", ""), 422, "VALIDATION_FAILED")
	groupPath := "/api/v2/admin/groups/" + created.ID
	renamed := decodeResponse[access.Group](t, admin.request("PATCH", groupPath, map[string]string{"name": "Renamed"}, resourceETag("group", created.ID, 1), ""), 200)
	if renamed.Version != 2 || renamed.Name != "Renamed" {
		t.Fatal("group rename did not publish its version")
	}
	expectCode(t, admin.request("DELETE", groupPath, nil, resourceETag("group", created.ID, 1), ""), 409, "VERSION_CONFLICT")
	if w := admin.request("DELETE", groupPath, nil, resourceETag("group", created.ID, 2), ""); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
}

func TestAccessHTTPScopesInvalidateCursorsAndInactiveLibrariesStayManageable(t *testing.T) {
	db, h := authFixture(t)
	admin, _ := ownerClient(t, h)
	user := createHTTPUser(t, admin, "reader", "user")
	viewer := accountClient(t, h, user.Username, testPassword)
	g, first, second := createHTTPGroup(t, admin, "Family"), createHTTPLibrary(t, admin, "First"), createHTTPLibrary(t, admin, "Second")
	membershipPath := "/api/v2/admin/users/" + user.ID + "/groups"
	decodeResponse[access.UserGroups](t, admin.request("PUT", membershipPath, map[string]any{"group_ids": []string{g.ID}}, userETag(user), ""), 200)
	for _, lib := range []access.Library{first, second} {
		path := "/api/v2/admin/libraries/" + lib.ID + "/access"
		decodeResponse[access.LibraryPolicy](t, admin.request("PUT", path, map[string]any{"group_ids": []string{g.ID}}, resourceETag("library", lib.ID, 1), ""), 200)
	}
	page := decodeResponse[listPage[access.Library]](t, viewer.request("GET", "/api/v2/libraries?limit=1", nil, "", ""), 200)
	if page.NextCursor == nil || !page.HasMore {
		t.Fatal("missing library cursor")
	}
	expectCode(t, viewer.request("GET", "/api/v2/albums?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, viewer.request("GET", "/api/v2/admin/groups", nil, "", ""), 403, "ADMIN_REQUIRED")
	path := "/api/v2/admin/libraries/" + first.ID
	inactive := decodeResponse[access.Library](t, admin.request("PATCH", path, map[string]bool{"active": false}, resourceETag("library", first.ID, 2), ""), 200)
	for _, client := range []adminClient{viewer, admin} {
		expectCode(t, client.request("GET", "/api/v2/libraries/"+first.ID, nil, "", ""), 404, "NOT_FOUND")
	}
	decodeResponse[access.Library](t, admin.request("GET", path, nil, "", ""), 200)
	expectCode(t, viewer.request("GET", "/api/v2/libraries?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	seedHTTPAlbum(t, db, first.ID, "Retained")
	expectCode(t, admin.request("DELETE", path, nil, resourceETag("library", first.ID, inactive.Version), ""), 409, "LIBRARY_NOT_EMPTY")
	libraries := decodeResponse[listPage[access.Library]](t, admin.request("GET", "/api/v2/admin/libraries", nil, "", ""), 200)
	ids := []string{}
	for _, lib := range libraries.Items {
		ids = append(ids, lib.ID)
	}
	if !slices.Contains(ids, first.ID) || !slices.Contains(ids, second.ID) {
		t.Fatal("management list omitted inactive library")
	}
	if w := admin.request("DELETE", "/api/v2/admin/libraries/"+second.ID, nil, resourceETag("library", second.ID, 2), ""); w.Code != 204 {
		t.Fatal("empty library could not be deleted", w.Body.String())
	}
}
