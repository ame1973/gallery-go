package httpapi

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

type sourceAPIFixture struct {
	db            *sql.DB
	handler       http.Handler
	admin         adminClient
	first, second access.Library
	roots         map[string]media.SourceRoot
}

func sourceAPI(t *testing.T) sourceAPIFixture {
	t.Helper()
	db, old := authFixture(t)
	admin, _ := ownerClient(t, old)
	a, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]media.SourceRoot{}
	for _, name := range []string{"photos", "other"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".marker"), []byte("isolated mount marker"), 0o400); err != nil {
			t.Fatal(err)
		}
		roots[name] = media.SourceRoot{Path: root, Marker: ".marker"}
	}
	roots["unmounted"] = media.SourceRoot{Path: filepath.Join(t.TempDir(), "missing"), Marker: ".marker"}
	h := New(db, fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>GalleryGo</title>")}}, "source-api", a, testOrigin, media.New(db, a, "", roots), testJobAdmin(t, db, a), testSourceAdmin(t, db, a, roots))
	admin.handler = h
	first := decodeResponse[access.Library](t, admin.request("POST", "/api/v2/admin/libraries", map[string]any{"name": "First"}, "", store.NewID()), 201)
	second := decodeResponse[access.Library](t, admin.request("POST", "/api/v2/admin/libraries", map[string]any{"name": "Second"}, "", store.NewID()), 201)
	return sourceAPIFixture{db, h, admin, first, second, roots}
}

func responseSource(t *testing.T, f sourceAPIFixture, w *httptest.ResponseRecorder, status int) library.Source {
	t.Helper()
	source := decodeResponse[library.Source](t, w, status)
	if source.ID == "" || w.Header().Get("ETag") != resourceETag("source", source.ID, source.Version) || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("source identity/version/privacy missing", w.Header())
	}
	for _, secret := range []string{f.roots["photos"].Path, f.roots["other"].Path, "root_path", "root_config_sha256", ".marker"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private source configuration leaked", secret)
		}
	}
	return source
}

func TestSourceHTTPCreateCheckMappingReplayAndEmptyDeletion(t *testing.T) {
	f := sourceAPI(t)
	input := map[string]any{"library_id": f.first.ID, "root_name": "photos", "mapping": map[string]int{"album_depth": 2, "person_depth": 1}, "minimum_files": 10}
	key := store.NewID()
	created := f.admin.request("POST", "/api/v2/admin/sources", input, "", key)
	source := responseSource(t, f, created, 201)
	path := "/api/v2/admin/sources/" + source.ID
	if !source.Active || !source.Configured || source.State != "unknown" || source.PollIntervalSeconds != 60 || source.FullScanIntervalSeconds != 86400 || source.Mapping != (library.Mapping{AlbumDepth: 2, PersonDepth: 1}) || created.Header().Get("Location") != path {
		t.Fatal(source, created.Header())
	}
	replayed := responseSource(t, f, f.admin.request("POST", "/api/v2/admin/sources", input, "", key), 201)
	if replayed.ID != source.ID || replayed.Version != source.Version {
		t.Fatal("source receipt duplicated creation")
	}
	expectCode(t, f.admin.request("POST", "/api/v2/admin/sources", input, "", store.NewID()), 409, "SOURCE_ROOT_IN_USE")
	probeKey := store.NewID()
	checked := responseSource(t, f, f.admin.request("POST", path+"/check", nil, created.Header().Get("ETag"), probeKey), 200)
	if checked.State != "healthy" || checked.ScanGeneration != 0 || checked.LastFullScanAt != nil || checked.ConfigVersion != source.ConfigVersion {
		t.Fatal("root check claimed completed inventory", checked)
	}
	probeReplay := responseSource(t, f, f.admin.request("POST", path+"/check", nil, created.Header().Get("ETag"), probeKey), 200)
	if probeReplay.Version != checked.Version {
		t.Fatal("source check replay repeated mutation")
	}
	noop := responseSource(t, f, f.admin.request("PATCH", path, map[string]int64{"minimum_files": 10}, resourceETag("source", source.ID, checked.Version), ""), 200)
	if noop.Version != checked.Version {
		t.Fatal("source no-op changed version")
	}
	queue, job := queuedHTTPJob(t, f.db, source.ID)
	updated := responseSource(t, f, f.admin.request("PATCH", path, map[string]any{"library_id": f.second.ID, "mapping": map[string]int{"album_depth": 3, "person_depth": 2}}, resourceETag("source", source.ID, noop.Version), ""), 200)
	if updated.ConfigVersion != 2 || updated.Version != checked.Version+1 || updated.State != "unknown" || updated.LastCheckedAt != nil {
		t.Fatal(updated)
	}
	cancelled, err := queue.Get(t.Context(), job.ID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatal("source change did not atomically invalidate work", cancelled, err)
	}
	removed := f.admin.request("DELETE", path, nil, resourceETag("source", source.ID, updated.Version), "")
	if removed.Code != 204 {
		t.Fatal(removed.Code, removed.Body.String())
	}
	expectCode(t, f.admin.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	marker, err := os.ReadFile(filepath.Join(f.roots["photos"].Path, ".marker"))
	if err != nil || string(marker) != "isolated mount marker" {
		t.Fatal("source deletion touched the read-only root", err)
	}
	defaults := responseSource(t, f, f.admin.request("POST", "/api/v2/admin/sources", map[string]any{"library_id": f.first.ID, "root_name": "photos"}, "", store.NewID()), 201)
	if defaults.Mapping != library.DefaultMapping() {
		t.Fatal("omitted mapping did not use creation defaults", defaults.Mapping)
	}
	explicitRoot := responseSource(t, f, f.admin.request("POST", "/api/v2/admin/sources", map[string]any{"library_id": f.first.ID, "root_name": "other", "mapping": map[string]any{}}, "", store.NewID()), 201)
	if explicitRoot.Mapping != (library.Mapping{}) {
		t.Fatal("explicit mapping object was not a full replacement", explicitRoot.Mapping)
	}
}

func TestSourceHTTPAuthorizationPrivateRootPagesAndMissingMounts(t *testing.T) {
	f := sourceAPI(t)
	created := f.admin.request("POST", "/api/v2/admin/sources", map[string]any{"library_id": f.first.ID, "root_name": "unmounted"}, "", store.NewID())
	source := responseSource(t, f, created, 201)
	path := "/api/v2/admin/sources/" + source.ID
	viewerUser := createHTTPUser(t, f.admin, "source-viewer", "user")
	viewer := accountClient(t, f.handler, viewerUser.Username, testPassword)
	for _, client := range []struct {
		value  adminClient
		status int
		code   string
	}{{adminClient{handler: f.handler}, 401, "UNAUTHENTICATED"}, {viewer, 403, "ADMIN_REQUIRED"}} {
		for _, route := range []struct{ method, path string }{{"GET", "/api/v2/admin/source-roots"}, {"GET", "/api/v2/admin/sources"}, {"POST", "/api/v2/admin/sources"}, {"GET", path}, {"PATCH", path}, {"DELETE", path}, {"POST", path + "/check"}} {
			expectCode(t, client.value.request(route.method, route.path, nil, "", store.NewID()), client.status, client.code)
		}
	}
	rootsResponse := f.admin.request("GET", "/api/v2/admin/source-roots?limit=1", nil, "", "")
	roots := decodeResponse[listPage[library.Root]](t, rootsResponse, 200)
	if len(roots.Items) != 1 || !roots.HasMore || roots.NextCursor == nil {
		t.Fatal(roots)
	}
	if strings.Contains(rootsResponse.Body.String(), f.roots["photos"].Path) {
		t.Fatal("root list leaked filesystem paths")
	}
	last := decodeResponse[listPage[library.Root]](t, f.admin.request("GET", "/api/v2/admin/source-roots?limit=100&cursor="+*roots.NextCursor, nil, "", ""), 200)
	if len(last.Items) != 2 || last.HasMore || last.Items[0].Name == roots.Items[0].Name {
		t.Fatal(last)
	}
	expectCode(t, f.admin.request("GET", "/api/v2/admin/sources?cursor="+*roots.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	checked := responseSource(t, f, f.admin.request("POST", path+"/check", nil, created.Header().Get("ETag"), store.NewID()), 200)
	if checked.State != "unavailable" || checked.LastErrorCode == nil || *checked.LastErrorCode != "SOURCE_UNAVAILABLE" {
		t.Fatal(checked)
	}
	if _, err := os.Stat(f.roots["unmounted"].Path); !os.IsNotExist(err) {
		t.Fatal("missing mount was created", err)
	}
	page := decodeResponse[listPage[library.Source]](t, f.admin.request("GET", "/api/v2/admin/sources", nil, "", ""), 200)
	if len(page.Items) != 1 || page.Items[0].ID != source.ID {
		t.Fatal(page)
	}
	responseSource(t, f, f.admin.request("PATCH", path, map[string]bool{"active": false}, resourceETag("source", source.ID, checked.Version), ""), 200)
	expectCode(t, f.admin.request("GET", "/api/v2/admin/source-roots?cursor="+*roots.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
}

func TestSourceHTTPStrictInputsPreconditionsAndAtomicFailure(t *testing.T) {
	f := sourceAPI(t)
	body := map[string]any{"library_id": f.first.ID, "root_name": "photos"}
	created := f.admin.request("POST", "/api/v2/admin/sources", body, "", store.NewID())
	source := responseSource(t, f, created, 201)
	path := "/api/v2/admin/sources/" + source.ID
	tag := created.Header().Get("ETag")
	for _, patch := range []any{
		map[string]any{"root_path": "/arbitrary/path"},
		map[string]any{"root_config_sha256": "forged"},
		map[string]any{"mapping": map[string]any{"Album_Depth": 2}},
		map[string]any{"mapping": map[string]any{"album_depth": nil}},
		map[string]any{"mapping": map[string]any{"unknown": 1}},
	} {
		expectCode(t, f.admin.request("PATCH", path, patch, tag, ""), 400, "INVALID_JSON")
	}
	for _, patch := range []any{map[string]any{}, map[string]any{"minimum_files": -1}, map[string]any{"poll_interval_seconds": 9}, map[string]any{"full_scan_interval_seconds": 10}, map[string]any{"mapping": map[string]int{"album_depth": 1, "person_depth": 2}}, map[string]any{"root_name": "https://not-a-local-source.example"}} {
		expectCode(t, f.admin.request("PATCH", path, patch, tag, ""), 422, "VALIDATION_FAILED")
	}
	expectCode(t, f.admin.request("PATCH", path, map[string]bool{"active": false}, "", ""), 428, "PRECONDITION_REQUIRED")
	expectCode(t, f.admin.request("PATCH", path, map[string]bool{"active": false}, resourceETag("source", source.ID, 99), ""), 409, "VERSION_CONFLICT")
	expectCode(t, f.admin.request("PATCH", path, map[string]bool{"active": false}, resourceETag("library", source.ID, 1), ""), 400, "INVALID_PRECONDITION")
	expectCode(t, f.admin.request("POST", path+"/check", map[string]any{}, tag, store.NewID()), 400, "INVALID_BODY")
	expectCode(t, f.admin.request("POST", path+"/check", nil, tag, ""), 422, "VALIDATION_FAILED")
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,display_name,created_at,updated_at) VALUES('retained-source-album',?,?,'Source','Override',0,0)`, f.first.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("DELETE", path, nil, tag, ""), 409, "SOURCE_NOT_EMPTY")
	queue, job := queuedHTTPJob(t, f.db, source.ID)
	before := decodeResponse[auth.User](t, f.admin.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_source_api_event BEFORE INSERT ON outbox_events WHEN (SELECT kind FROM change_log WHERE seq=NEW.change_seq)='source' BEGIN SELECT RAISE(ABORT,'private source diagnostic'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("PATCH", path, map[string]bool{"active": false}, tag, "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(failed.Body.String(), "private source diagnostic") {
		t.Fatal("raw source failure leaked")
	}
	current := responseSource(t, f, f.admin.request("GET", path, nil, "", ""), 200)
	if current.Version != source.Version || !current.Active {
		t.Fatal("failed source update partially committed", current)
	}
	after := decodeResponse[auth.User](t, f.admin.request("GET", "/api/v2/auth/me", nil, "", ""), 200)
	if after.ScopeID != before.ScopeID {
		t.Fatal("scope revocation survived API rollback")
	}
	pending, err := queue.Get(t.Context(), job.ID)
	if err != nil || pending.Status != "queued" || pending.CancelRequested {
		t.Fatal("job cancellation survived API rollback", pending, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_source_api_event`); err != nil {
		t.Fatal(err)
	}
	moved := responseSource(t, f, f.admin.request("PATCH", path, map[string]string{"library_id": f.second.ID}, tag, ""), 200)
	if moved.LibraryID != f.second.ID {
		t.Fatal(moved)
	}
	expectCode(t, f.admin.request("GET", "/api/v2/albums/retained-source-album", nil, "", ""), 404, "NOT_FOUND")
	var title string
	if err := f.db.QueryRow(`SELECT display_name FROM albums WHERE id='retained-source-album'`).Scan(&title); err != nil || title != "Override" {
		t.Fatal("library reassignment overwrote indexed metadata", title, err)
	}
}
