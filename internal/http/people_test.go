package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func personMetadataHTTPFixture(t *testing.T) protectedFixture {
	t.Helper()
	f := protectedMedia(t)
	if _, err := f.db.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('metadata-person','media-source','Private/Creator','Folder creator',0); UPDATE albums SET person_id='metadata-person' WHERE id='media-album'`); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPersonMetadataHTTPVersionsHistoricalReceiptsAndPublicProjection(t *testing.T) {
	f := personMetadataHTTPFixture(t)
	const path = "/api/v2/admin/people/metadata-person"
	initial := f.admin.request("GET", path, nil, "", "")
	before := decodeResponse[access.PersonMetadata](t, initial, 200)
	if initial.Header().Get("ETag") != `"person:metadata-person:1"` || initial.Header().Get("Cache-Control") != "private, no-store" || before.SourceName != "Folder creator" || before.SourceID != "media-source" || before.DisplayName != nil {
		t.Fatal("admin person metadata did not return bounded versioned preferences", before, initial.Header())
	}
	var originalScope string
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.user.ID).Scan(&originalScope); err != nil {
		t.Fatal(err)
	}
	key := store.NewID()
	patch := map[string]any{"display_name": "  Edited creator 字  "}
	response := f.admin.request("PATCH", path, patch, initial.Header().Get("ETag"), key)
	changed := decodeResponse[access.PersonMetadata](t, response, 200)
	if changed.Version != 2 || changed.DisplayName == nil || *changed.DisplayName != "Edited creator 字" || changed.SourceName != before.SourceName || changed.SourceID != before.SourceID || response.Header().Get("ETag") != `"person:metadata-person:2"` {
		t.Fatal("person PATCH lost its normalized label or exact version", changed, response.Header())
	}
	public := decodeResponse[access.Person](t, f.reader.request("GET", "/api/v2/people/metadata-person", nil, "", ""), 200)
	if public.Name != *changed.DisplayName || public.Version != 2 || public.AlbumCount != 1 || public.CoverMediaID == nil || *public.CoverMediaID != "protected" {
		t.Fatal("public person name/count/cover projection changed incorrectly", public)
	}
	listed := decodeResponse[listPage[access.Person]](t, f.reader.request("GET", "/api/v2/people", nil, "", ""), 200)
	if len(listed.Items) != 1 || !reflect.DeepEqual(listed.Items[0], public) {
		t.Fatal("person list/detail disagreed after label update", listed, public)
	}
	noop := decodeResponse[access.PersonMetadata](t, f.admin.request("PATCH", path, patch, response.Header().Get("ETag"), store.NewID()), 200)
	if !reflect.DeepEqual(noop, changed) {
		t.Fatal("same person label preference changed its version", noop, changed)
	}
	clearResponse := f.admin.request("PATCH", path, map[string]any{"display_name": ""}, response.Header().Get("ETag"), store.NewID())
	cleared := decodeResponse[access.PersonMetadata](t, clearResponse, 200)
	if cleared.DisplayName != nil || cleared.Version != 3 || cleared.SourceName != before.SourceName {
		t.Fatal("explicit empty label did not clear only the override", cleared)
	}
	public = decodeResponse[access.Person](t, f.reader.request("GET", "/api/v2/people/metadata-person", nil, "", ""), 200)
	if public.Name != before.SourceName || public.Version != 3 {
		t.Fatal("cleared person name did not use its source label", public)
	}
	replayed := f.admin.request("PATCH", path, patch, initial.Header().Get("ETag"), key)
	if value := decodeResponse[access.PersonMetadata](t, replayed, 200); !reflect.DeepEqual(value, changed) || replayed.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("historical receipt changed after a later person edit", value, changed)
	}
	expectCode(t, f.admin.request("PATCH", path, patch, initial.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	expectCode(t, f.admin.request("PATCH", path, map[string]any{"display_name": "Different"}, initial.Header().Get("ETag"), key), 409, "IDEMPOTENCY_CONFLICT")
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	unmounted := decodeResponse[access.PersonMetadata](t, f.admin.request("PATCH", path, map[string]any{"display_name": "Offline source label"}, clearResponse.Header().Get("ETag"), store.NewID()), 200)
	if unmounted.Version != 4 || unmounted.SourceName != before.SourceName || unmounted.SourceID != before.SourceID {
		t.Fatal("person mutation probed or reassigned an unavailable source", unmounted)
	}
	var currentScope string
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.user.ID).Scan(&currentScope); err != nil || currentScope != originalScope {
		t.Fatal("person label mutation changed authorization scope", originalScope, currentScope, err)
	}
	for _, secret := range []string{f.source, "relative_path", "root_path", "marker", "source_config", "Private/Creator"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("person metadata exposed a private source identity/path", secret)
		}
	}
}

func TestPersonMetadataHTTPRejectsUnsafeBodiesRolesPreconditionsAndLateFailures(t *testing.T) {
	f := personMetadataHTTPFixture(t)
	const path = "/api/v2/admin/people/metadata-person"
	tag := `"person:metadata-person:1"`
	for _, client := range []struct {
		value  adminClient
		status int
		code   string
	}{{adminClient{handler: f.http}, 401, "UNAUTHENTICATED"}, {f.reader, 403, "ADMIN_REQUIRED"}, {adminClient{handler: f.http, cookie: &http.Cookie{Name: sessionCookie, Value: strings.Repeat("x", 43)}}, 401, "UNAUTHENTICATED"}} {
		for _, route := range []string{path, "/api/v2/admin/people/missing"} {
			expectCode(t, client.value.request("GET", route, nil, "", ""), client.status, client.code)
			expectCode(t, client.value.request("PATCH", route, map[string]any{"display_name": "Denied"}, tag, store.NewID()), client.status, client.code)
		}
	}
	expectCode(t, f.admin.request("PATCH", path, map[string]any{"display_name": "Name"}, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
	for _, invalid := range []string{"*", `W/"person:metadata-person:1"`, `"person:other:1"`, `"album:metadata-person:1"`, `"person:metadata-person:0"`} {
		expectCode(t, f.admin.request("PATCH", path, map[string]any{"display_name": "Name"}, invalid, store.NewID()), 400, "INVALID_PRECONDITION")
	}
	expectCode(t, f.admin.request("PATCH", path, map[string]any{"display_name": "Name"}, tag, ""), 422, "VALIDATION_FAILED")
	for _, body := range []map[string]any{{"display_name": nil}, {"display_name": true}, {"source_name": "rename source"}, {"source_id": "other"}, {"relative_path": "other"}, {"album_id": "other"}, {"Display_Name": "ambiguous"}, {"cover_media_id": "protected"}} {
		expectCode(t, f.admin.request("PATCH", path, body, tag, store.NewID()), 400, "INVALID_JSON")
	}
	for _, body := range []map[string]any{{}, {"display_name": "   "}, {"display_name": strings.Repeat("字", 101)}, {"display_name": "bad\x00name"}} {
		expectCode(t, f.admin.request("PATCH", path, body, tag, store.NewID()), 422, "VALIDATION_FAILED")
	}
	expectCode(t, requestAuth(f.http, "PATCH", path, `{"display_name":"Denied"}`, testOrigin, "", f.admin.cookie), 403, "CSRF_REJECTED")
	expectCode(t, f.admin.request("GET", "/api/v2/admin/people/missing", nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.admin.request("PATCH", "/api/v2/admin/people/missing", map[string]any{"display_name": "Name"}, `"person:missing:1"`, store.NewID()), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`CREATE TRIGGER reject_person_metadata_receipt BEFORE INSERT ON mutation_receipts BEGIN SELECT RAISE(ABORT,'/private/SQL/receipt'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("PATCH", path, map[string]any{"display_name": "Must roll back"}, tag, store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(failed.Body.String(), "/private") || strings.Contains(failed.Body.String(), "SQL") || strings.Contains(failed.Body.String(), "receipt") {
		t.Fatal("late person journal failure exposed internal details", failed.Body.String())
	}
	value := decodeResponse[access.PersonMetadata](t, f.admin.request("GET", path, nil, "", ""), 200)
	if value.Version != 1 || value.DisplayName != nil {
		t.Fatal("failed person receipt left a partial row/version", value)
	}
	if _, err := f.db.Exec(`UPDATE sources SET is_active=0 WHERE id='media-source'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.admin.request("PATCH", path, map[string]any{"display_name": "Denied"}, tag, store.NewID()), 404, "NOT_FOUND")
}

func TestPeopleHTTPPagesUseCurrentVisibilityAndScopeCollectionBoundCursors(t *testing.T) {
	f := protectedMedia(t)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 105 {
		id := fmt.Sprintf("person-%03d", index)
		if _, err := tx.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES(?,'media-source',?,'Same creator',0)`, id, "private/"+id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES(?,'media-library','media-source',?,'Album',0,0)`, "album-"+id, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v2/people"
	response := f.reader.request("GET", path, nil, "", "")
	page := decodeResponse[listPage[access.Person]](t, response, 200)
	if len(page.Items) != 30 || !page.HasMore || page.NextCursor == nil || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("person page was not bounded/private", page, response.Header())
	}
	first := decodeResponse[listPage[access.Person]](t, f.reader.request("GET", path+"?limit=100", nil, "", ""), 200)
	last := decodeResponse[listPage[access.Person]](t, f.reader.request("GET", path+"?limit=100&cursor="+*first.NextCursor, nil, "", ""), 200)
	if len(first.Items) != 100 || len(last.Items) != 5 || last.HasMore || last.NextCursor != nil || last.Items[4].ID != "person-104" {
		t.Fatal("person cursor lost/duplicated rows", first, last)
	}
	person := decodeResponse[access.Person](t, f.reader.request("GET", path+"/person-000", nil, "", ""), 200)
	if person.Name != "Same creator" || person.SourceID != "media-source" || person.AlbumCount != 1 || person.CoverMediaID != nil || person.Version != 1 {
		t.Fatal("person detail does not match authorized list projection", person)
	}
	for _, private := range []string{"relative_path", "root_path", "private/person", f.source, "marker"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatal("person API exposed private mapping/paths", private)
		}
	}
	if _, err := f.db.Exec(`UPDATE albums SET person_id='person-000' WHERE id LIKE 'album-person-%'`); err != nil {
		t.Fatal(err)
	}
	memberPath := path + "/person-000/albums"
	members := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", memberPath+"?limit=100", nil, "", ""), 200)
	if len(members.Items) != 100 || !members.HasMore || members.NextCursor == nil {
		t.Fatal("person album page is not bounded", members)
	}
	end := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", memberPath+"?limit=100&cursor="+*members.NextCursor, nil, "", ""), 200)
	if len(end.Items) != 5 || end.HasMore || end.NextCursor != nil {
		t.Fatal("person album keyset lost rows", end)
	}
	for _, route := range []string{path + "/person-001/albums", "/api/v2/albums", memberPath} {
		expectCode(t, f.reader.request("GET", route+"?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	}
	expectCode(t, f.reader.request("GET", path+"?cursor="+*members.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	for _, route := range []string{path, memberPath} {
		for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "cursor=x&cursor=y"} {
			expectCode(t, f.reader.request("GET", route+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
		}
		expectCode(t, f.reader.request("GET", route+"?source_id=media-source", nil, "", ""), 400, "INVALID_FILTER")
	}
	expectCode(t, f.reader.request("GET", path+"/missing", nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.reader.request("GET", path+"/missing/albums", nil, "", ""), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`UPDATE users SET scope_id='rotated-person-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, f.reader.request("GET", memberPath+"?cursor="+*members.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
}

func TestPeopleHTTPRejectsAnonymousHiddenInactiveAndBrokenQueryContexts(t *testing.T) {
	f := protectedMedia(t)
	if _, err := f.db.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,display_name,created_at) VALUES('visible-person','media-source','private/person','Folder label','Visible creator',0),('empty-person','media-source','private/empty','Empty',NULL,0);
 UPDATE albums SET person_id='visible-person' WHERE id='media-album';
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,access_restricted,created_at,updated_at) VALUES('hidden-person-album','media-library','media-source','visible-person','Hidden',1,0,0);
 INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('0-hidden-person-cover','hidden-person-album','image','hidden.jpg',0,0);
 UPDATE albums SET cover_media_id='0-hidden-person-cover' WHERE id='hidden-person-album'`); err != nil {
		t.Fatal(err)
	}
	for _, client := range []adminClient{{handler: f.http}, {handler: f.http, cookie: &http.Cookie{Name: sessionCookie, Value: strings.Repeat("x", 43)}}} {
		for _, path := range []string{"/api/v2/people", "/api/v2/people/visible-person", "/api/v2/people/visible-person/albums"} {
			expectCode(t, client.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
		}
	}
	person := decodeResponse[access.Person](t, f.reader.request("GET", "/api/v2/people/visible-person", nil, "", ""), 200)
	if person.AlbumCount != 1 || person.CoverMediaID == nil || *person.CoverMediaID != "protected" || person.Name != "Visible creator" {
		t.Fatal("hidden albums/media affected public person aggregates", person)
	}
	adminPerson := decodeResponse[access.Person](t, f.admin.request("GET", "/api/v2/people/visible-person", nil, "", ""), 200)
	if adminPerson.AlbumCount != 2 || adminPerson.CoverMediaID == nil || *adminPerson.CoverMediaID != "0-hidden-person-cover" {
		t.Fatal("administrator person projection lost valid indexed albums", adminPerson)
	}
	for _, path := range []string{"/api/v2/people/empty-person", "/api/v2/people/empty-person/albums"} {
		expectCode(t, f.reader.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	}
	if _, err := f.db.Exec(`UPDATE libraries SET is_active=0 WHERE id='media-library'`); err != nil {
		t.Fatal(err)
	}
	for _, client := range []adminClient{f.reader, f.admin} {
		page := decodeResponse[listPage[access.Person]](t, client.request("GET", "/api/v2/people", nil, "", ""), 200)
		if page.Items == nil || len(page.Items) != 0 || page.HasMore || page.NextCursor != nil {
			t.Fatal("inactive library disclosed creator existence", page)
		}
		expectCode(t, client.request("GET", "/api/v2/people/visible-person", nil, "", ""), 404, "NOT_FOUND")
	}
	if _, err := f.db.Exec(`UPDATE libraries SET is_active=1; ALTER TABLE people RENAME TO private_broken_people`); err != nil {
		t.Fatal(err)
	}
	failed := f.reader.request("GET", "/api/v2/people", nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(failed.Body.String(), "people") || strings.Contains(failed.Body.String(), "SELECT") || strings.Contains(failed.Body.String(), "/private") {
		t.Fatal("person query failure exposed internal schema/path", failed.Body.String())
	}
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", "/api/v2/people", nil, "", ""), 401, "UNAUTHENTICATED")
}
