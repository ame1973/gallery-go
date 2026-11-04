package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/store"
)

func TestScanHTTPSelectionNullabilityReplayAndConfigurationCancellation(t *testing.T) {
	f := sourceAPI(t)
	source := decodeResponse[library.Source](t, f.admin.request("POST", "/api/v2/admin/sources", map[string]any{"library_id": f.first.ID, "root_name": "photos"}, "", store.NewID()), 201)
	key := store.NewID()
	body := map[string]any{"source_id": source.ID, "mode": "full"}
	accepted := f.admin.request("POST", "/api/v2/admin/scans", body, "", key)
	first := decodeResponse[jobs.Job](t, accepted, 202)
	if first.Status != "queued" || first.Attempts != 0 || first.TotalKnown || accepted.Header().Get("Location") != "/api/v2/admin/jobs/"+first.ID || accepted.Header().Get("ETag") != resourceETag("job", first.ID, first.Version) {
		t.Fatal("invalid scan acceptance", first, accepted.Header())
	}
	for _, private := range []string{f.roots["photos"].Path, "scan_targets", "scope_path", "source_config_version", "lease_owner", "payload"} {
		if strings.Contains(accepted.Body.String(), private) {
			t.Fatal("private enqueue metadata leaked", private)
		}
	}
	coalesced := decodeResponse[jobs.Job](t, f.admin.request("POST", "/api/v2/admin/scans", body, "", store.NewID()), 202)
	if coalesced.ID != first.ID {
		t.Fatal("same intent did not coalesce", coalesced)
	}
	allKey := store.NewID()
	all := decodeResponse[jobs.Job](t, f.admin.request("POST", "/api/v2/admin/scans", map[string]any{"source_id": nil, "album_id": nil, "mode": "full"}, "", allKey), 202)
	if all.ID == first.ID {
		t.Fatal("all-source and source intents coalesced")
	}
	nullReplay := decodeResponse[jobs.Job](t, f.admin.request("POST", "/api/v2/admin/scans", map[string]any{"mode": "full"}, "", allKey), 202)
	if nullReplay.ID != all.ID {
		t.Fatal("omitted/null canonical selection differs", nullReplay)
	}
	album := store.NewID()
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,relative_path,created_at,updated_at) VALUES(?,?,?,'Target','Target',0,0)`, album, f.first.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	target := decodeResponse[jobs.Job](t, f.admin.request("POST", "/api/v2/admin/scans", map[string]any{"album_id": album, "mode": "incremental"}, "", store.NewID()), 202)
	if target.ID == first.ID || target.ID == all.ID {
		t.Fatal("target intent coalesced with another selection")
	}
	updated := decodeResponse[library.Source](t, f.admin.request("PATCH", "/api/v2/admin/sources/"+source.ID, map[string]any{"minimum_files": 5}, resourceETag("source", source.ID, source.Version), ""), 200)
	if updated.ConfigVersion != source.ConfigVersion+1 || updated.ScanGeneration != 0 || updated.State != "unknown" {
		t.Fatal("enqueue claimed ingestion/health", updated)
	}
	for _, job := range []jobs.Job{first, all, target} {
		current := decodeResponse[jobs.Job](t, f.admin.request("GET", "/api/v2/admin/jobs/"+job.ID, nil, "", ""), 200)
		if current.Status != "cancelled" {
			t.Fatal("source edit failed to cancel a related scan intent", current)
		}
	}
	replay := decodeResponse[jobs.Job](t, f.admin.request("POST", "/api/v2/admin/scans", body, "", key), 202)
	if replay.ID != first.ID || replay.Status != "queued" {
		t.Fatal("accepted-result replay lost historical semantics", replay)
	}
	expectCode(t, f.admin.request("POST", "/api/v2/admin/scans", map[string]any{"mode": "incremental", "source_id": source.ID}, "", key), 409, "IDEMPOTENCY_CONFLICT")
}

func TestScanHTTPSafetyStrictBodiesAndAdministrativeBoundary(t *testing.T) {
	f := sourceAPI(t)
	expectCode(t, requestAuth(f.handler, "POST", "/api/v2/admin/scans", `{"mode":"full"}`, testOrigin, "", nil), 401, "UNAUTHENTICATED")
	expectCode(t, requestAuth(f.handler, "POST", "/api/v2/admin/scans", `{"mode":"full"}`, testOrigin, "", f.admin.cookie), 403, "CSRF_REJECTED")
	expectCode(t, f.admin.request("POST", "/api/v2/admin/scans", map[string]any{"mode": "full"}, "", ""), 422, "VALIDATION_FAILED")
	expectCode(t, f.admin.request("POST", "/api/v2/admin/scans", map[string]any{"mode": "full"}, "", store.NewID()), 409, "SCAN_NO_SOURCES")
	for _, body := range []string{`{"Mode":"full"}`, `{"mode":null}`, `{"mode":"full","Source_ID":null}`, `{"mode":"full","source_id":{}}`, `{"mode":"full","album_id":0}`, `{"mode":"full","root_path":"/private"}`, `{"mode":"full","mode":"incremental"}`} {
		r := httptest.NewRequest("POST", "/api/v2/admin/scans", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("X-CSRF-Token", f.admin.csrf)
		r.Header.Set("Idempotency-Key", store.NewID())
		r.AddCookie(f.admin.cookie)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		expectCode(t, w, 400, "INVALID_JSON")
	}
	for _, body := range []map[string]any{{}, {"mode": "FULL"}, {"mode": "full", "source_id": ""}} {
		expectCode(t, f.admin.request("POST", "/api/v2/admin/scans", body, "", store.NewID()), 422, "VALIDATION_FAILED")
	}
	expectCode(t, f.admin.request("POST", "/api/v2/admin/scans", map[string]any{"mode": "full", "source_id": "unknown"}, "", store.NewID()), 404, "NOT_FOUND")
	user := createHTTPUser(t, f.admin, "scan-reader", "user")
	reader := accountClient(t, f.handler, user.Username, testPassword)
	expectCode(t, reader.request("POST", "/api/v2/admin/scans", map[string]any{"mode": "full"}, "", store.NewID()), 403, "ADMIN_REQUIRED")
}
