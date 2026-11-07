package httpapi

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func albumCreatorHTTPFixture(t *testing.T) protectedFixture {
	t.Helper()
	f := protectedMedia(t)
	if _, err := f.db.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('mapped-creator','media-source','Mapped','Mapped label',0),('manual-creator','media-source','Manual','Manual label',0),('unbound-creator','media-source',NULL,'Unbound label',0);
 UPDATE albums SET person_id='mapped-creator',mapped_person_id='mapped-creator' WHERE id='media-album'`); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAlbumCreatorHTTPModesHistoryScopesAndSourceOutage(t *testing.T) {
	f := albumCreatorHTTPFixture(t)
	const path = "/api/v2/admin/albums/media-album/creator"
	initial := f.admin.request("GET", path, nil, "", "")
	before := decodeResponse[access.AlbumCreator](t, initial, 200)
	if before.Mode != "mapped" || before.PersonID == nil || *before.PersonID != "mapped-creator" || before.MappedPersonID == nil || *before.MappedPersonID != "mapped-creator" || before.OverridePersonID != nil || initial.Header().Get("ETag") != `"album:media-album:1"` || initial.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("incorrect versioned creator preferences", before, initial.Header())
	}
	meBefore := decodeResponse[auth.User](t, f.reader.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	key := store.NewID()
	body := map[string]any{"mode": "person", "person_id": "manual-creator"}
	response := f.admin.request("PUT", path, body, initial.Header().Get("ETag"), key)
	changed := decodeResponse[access.AlbumCreator](t, response, 200)
	if changed.Version != 2 || changed.Mode != "person" || changed.PersonID == nil || *changed.PersonID != "manual-creator" || changed.MappedPersonID == nil || *changed.MappedPersonID != "mapped-creator" || changed.OverridePersonID == nil || *changed.OverridePersonID != "manual-creator" {
		t.Fatal("manual association lost provenance or version", changed)
	}
	meAfter := decodeResponse[auth.User](t, f.reader.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	if meAfter.ScopeID == meBefore.ScopeID {
		t.Fatal("creator graph removal did not invalidate old aggregate/snapshot scope")
	}
	expectCode(t, f.reader.request("GET", "/api/v2/people/mapped-creator", nil, "", ""), 404, "NOT_FOUND")
	person := decodeResponse[access.Person](t, f.reader.request("GET", "/api/v2/people/manual-creator", nil, "", ""), 200)
	if person.Name != "Manual label" || person.AlbumCount != 1 {
		t.Fatal("public creator projection did not follow current classification", person)
	}
	noop := decodeResponse[access.AlbumCreator](t, f.admin.request("PUT", path, body, response.Header().Get("ETag"), store.NewID()), 200)
	if !reflect.DeepEqual(noop, changed) {
		t.Fatal("identical creator preference changed version", noop, changed)
	}
	meNoop := decodeResponse[auth.User](t, f.reader.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	if meNoop.ScopeID != meAfter.ScopeID {
		t.Fatal("creator no-op rotated unchanged scopes")
	}
	noneResponse := f.admin.request("PUT", path, map[string]string{"mode": "none"}, response.Header().Get("ETag"), store.NewID())
	none := decodeResponse[access.AlbumCreator](t, noneResponse, 200)
	if none.Mode != "none" || none.PersonID != nil || none.OverridePersonID != nil || none.MappedPersonID == nil || none.Version != 3 {
		t.Fatal("explicit no-creator mode lost indexed fallback", none)
	}
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	restoredResponse := f.admin.request("PUT", path, map[string]string{"mode": "mapped"}, noneResponse.Header().Get("ETag"), store.NewID())
	restored := decodeResponse[access.AlbumCreator](t, restoredResponse, 200)
	if restored.PersonID == nil || *restored.PersonID != "mapped-creator" || restored.Mode != "mapped" || restored.OverridePersonID != nil || restored.Version != 4 {
		t.Fatal("mapped restoration probed missing source or lost provenance", restored)
	}
	replayed := f.admin.request("PUT", path, body, initial.Header().Get("ETag"), key)
	if value := decodeResponse[access.AlbumCreator](t, replayed, 200); !reflect.DeepEqual(value, changed) || replayed.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("creator history was retargeted by newer choices", value, changed)
	}
	expectCode(t, f.admin.request("PUT", path, body, initial.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	expectCode(t, f.admin.request("PUT", path, map[string]string{"mode": "none"}, initial.Header().Get("ETag"), key), 409, "IDEMPOTENCY_CONFLICT")
	for _, secret := range []string{f.source, "root_path", "relative_path", "source_config", "Mapped/", "Manual/"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("creator preference exposed private paths/pins", secret)
		}
	}
}

func TestAlbumCreatorHTTPRejectsUnsafeBodiesRolesAndLateJournalFailure(t *testing.T) {
	f := albumCreatorHTTPFixture(t)
	const path = "/api/v2/admin/albums/media-album/creator"
	tag := `"album:media-album:1"`
	guest := adminClient{handler: f.http}
	for _, route := range []string{path, "/api/v2/admin/albums/missing/creator"} {
		expectCode(t, guest.request("GET", route, nil, "", ""), 401, "UNAUTHENTICATED")
		expectCode(t, f.reader.request("GET", route, nil, "", ""), 403, "ADMIN_REQUIRED")
		expectCode(t, f.reader.request("PUT", route, map[string]string{"mode": "none"}, tag, store.NewID()), 403, "ADMIN_REQUIRED")
	}
	for _, body := range []map[string]any{{"mode": nil}, {"mode": "person", "person_id": nil}, {"mode": true}, {"Mode": "mapped"}, {"source_id": "other"}, {"mapped_person_id": "other"}, {"restricted": false}, {"version": 5}} {
		expectCode(t, f.admin.request("PUT", path, body, tag, store.NewID()), 400, "INVALID_JSON")
	}
	for _, body := range []map[string]any{{}, {"mode": "unknown"}, {"mode": "person"}, {"mode": "person", "person_id": ""}, {"mode": "person", "person_id": "missing"}, {"mode": "person", "person_id": "unbound-creator"}, {"mode": "none", "person_id": "manual-creator"}, {"mode": "mapped", "person_id": ""}} {
		expectCode(t, f.admin.request("PUT", path, body, tag, store.NewID()), 422, "VALIDATION_FAILED")
	}
	expectCode(t, f.admin.request("PUT", path, map[string]string{"mode": "none"}, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
	expectCode(t, f.admin.request("PUT", path, map[string]string{"mode": "none"}, tag, ""), 422, "VALIDATION_FAILED")
	for _, invalid := range []string{"*", "W/" + tag, `"album:other:1"`, `"person:media-album:1"`} {
		expectCode(t, f.admin.request("PUT", path, map[string]string{"mode": "none"}, invalid, store.NewID()), 400, "INVALID_PRECONDITION")
	}
	expectCode(t, requestAuth(f.http, "PUT", path, `{"mode":"none"}`, testOrigin, "", f.admin.cookie), 403, "CSRF_REJECTED")
	before := decodeResponse[auth.User](t, f.reader.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_creator_receipt BEFORE INSERT ON mutation_receipts BEGIN SELECT RAISE(ABORT,'/private/SQL/creator'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("PUT", path, map[string]string{"mode": "none"}, tag, store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(problemContent(t, failed), "/private") || strings.Contains(problemContent(t, failed), "SQL") || strings.Contains(problemContent(t, failed), "creator") {
		t.Fatal("creator journal error exposed internals", failed.Body.String())
	}
	current := decodeResponse[access.AlbumCreator](t, f.admin.request("GET", path, nil, "", ""), 200)
	after := decodeResponse[auth.User](t, f.reader.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	if current.Version != 1 || current.Mode != "mapped" || current.PersonID == nil || *current.PersonID != "mapped-creator" || after.ScopeID != before.ScopeID {
		t.Fatal("failed creator receipt left graph/version/scope changes", current, before.ScopeID, after.ScopeID)
	}
	if _, err := f.db.Exec(`UPDATE sources SET is_active=0 WHERE id='media-source'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.admin.request("PUT", path, map[string]string{"mode": "none"}, tag, store.NewID()), 404, "NOT_FOUND")
}
