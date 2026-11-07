package httpapi

import (
	"fmt"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/store"
)

func TestTagBatchHTTPAcceptanceHistoryAndSourceConfigurationCancellation(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	const path = "/api/v2/admin/batches/tags"
	key := store.NewID()
	body := map[string]any{"album_ids": []string{"other-tag-album", "media-album"}, "tag_ids": []string{"tag-two", "tag-one"}}
	// A missing mount must not turn this DB-only acceptance into source I/O.
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	response := f.admin.request("POST", path, body, "", key)
	accepted := decodeResponse[jobs.Job](t, response, 202)
	if accepted.Kind != "batch_tags" || accepted.Status != "queued" || accepted.Attempts != 0 || accepted.TotalKnown || accepted.Counts != (jobs.Counts{}) || response.Header().Get("Location") != "/api/v2/admin/jobs/"+accepted.ID || response.Header().Get("ETag") != resourceETag("job", accepted.ID, 1) || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("enqueue advertised processing/completion or omitted protected job identity", accepted, response.Header())
	}
	targetPath := "/api/v2/admin/batches/" + accepted.ID + "/targets"
	targetResponse := f.admin.request("GET", targetPath, nil, "", "")
	targets := decodeResponse[listPage[access.TagBatchTarget]](t, targetResponse, 200)
	if len(targets.Items) != 2 || targets.HasMore || targets.NextCursor != nil || targets.Items[0].AlbumID != "media-album" || targets.Items[0].AlbumVersion != 1 || targets.Items[0].SourceConfigVersion == nil || *targets.Items[0].SourceConfigVersion != 1 || !reflect.DeepEqual(targets.Items[0].TagIDs, []string{"tag-one", "tag-two"}) {
		t.Fatal("target query did not preserve sorted exact epochs/tag selection", targets)
	}
	for _, secret := range []string{f.source, "root_path", "relative_path", "payload_json", "lease_owner", "root_config_sha256", "Hidden private label"} {
		if strings.Contains(response.Body.String()+targetResponse.Body.String(), secret) {
			t.Fatal("batch metadata exposed private content/authority", secret)
		}
	}
	coalesced := decodeResponse[jobs.Job](t, f.admin.request("POST", path, map[string]any{"album_ids": []string{"media-album", "other-tag-album"}, "tag_ids": []string{"tag-one", "tag-two"}}, "", store.NewID()), 202)
	if coalesced.ID != accepted.ID {
		t.Fatal("canonical active selection did not coalesce", coalesced)
	}
	unchanged := decodeResponse[access.AlbumTagSet](t, f.admin.request("GET", "/api/v2/admin/albums/media-album/tags", nil, "", ""), 200)
	if unchanged.Version != 1 || len(unchanged.TagIDs) != 0 {
		t.Fatal("acceptance applied album tags", unchanged)
	}
	source := decodeResponse[library.Source](t, f.admin.request("GET", "/api/v2/admin/sources/media-source", nil, "", ""), 200)
	updated := decodeResponse[library.Source](t, f.admin.request("PATCH", "/api/v2/admin/sources/media-source", map[string]any{"minimum_files": 5}, resourceETag("source", source.ID, source.Version), ""), 200)
	if updated.ConfigVersion != 2 {
		t.Fatal("source epoch fixture failed", updated)
	}
	current := decodeResponse[jobs.Job](t, f.admin.request("GET", response.Header().Get("Location"), nil, "", ""), 200)
	if current.Status != "cancelled" || current.Succeeded != 0 || current.Attempts != 0 {
		t.Fatal("source edit did not cancel the bound queued batch truthfully", current)
	}
	history := decodeResponse[listPage[access.TagBatchTarget]](t, f.admin.request("GET", targetPath, nil, "", ""), 200)
	if !reflect.DeepEqual(history, targets) {
		t.Fatal("source edit mutated historical targets", history, targets)
	}
	replayed := f.admin.request("POST", path, body, "", key)
	if old := decodeResponse[jobs.Job](t, replayed, 202); old.ID != accepted.ID || old.Status != "queued" || replayed.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("historical receipt was mistaken for current job state", old)
	}
	expectCode(t, f.admin.request("POST", path, map[string]any{"album_ids": []string{"media-album"}, "tag_ids": []string{}}, "", key), 409, "IDEMPOTENCY_CONFLICT")
	newJob := decodeResponse[jobs.Job](t, f.admin.request("POST", path, body, "", store.NewID()), 202)
	if newJob.ID == accepted.ID {
		t.Fatal("new source epoch retargeted the old job", newJob)
	}
	expectCode(t, f.admin.request("GET", "/api/v2/admin/batches/missing/targets", nil, "", ""), 404, "NOT_FOUND")
}

func TestTagBatchHTTPStrictBodiesFreshRolesAndSanitizedLateRollback(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	const path = "/api/v2/admin/batches/tags"
	valid := map[string]any{"album_ids": []string{"media-album"}, "tag_ids": []string{}}
	guest := adminClient{handler: f.http}
	for _, client := range []adminClient{guest, f.reader} {
		status, code := 403, "ADMIN_REQUIRED"
		if client.cookie == nil {
			status, code = 401, "UNAUTHENTICATED"
		}
		expectCode(t, client.request("POST", path, valid, "", store.NewID()), status, code)
		expectCode(t, client.request("GET", "/api/v2/admin/batches/missing/targets", nil, "", ""), status, code)
	}
	expectCode(t, requestAuth(f.http, "POST", path, `{"album_ids":["media-album"],"tag_ids":[]}`, testOrigin, "", f.admin.cookie), 403, "CSRF_REJECTED")
	expectCode(t, requestAuth(f.http, "POST", path, `{"album_ids":["media-album"],"tag_ids":[]}`, "https://foreign.test", f.admin.csrf, f.admin.cookie), 403, "ORIGIN_REJECTED")
	for _, body := range []map[string]any{
		{"album_ids": nil, "tag_ids": []string{}}, {"album_ids": []string{"media-album"}, "tag_ids": nil},
		{"Album_IDs": []string{}, "tag_ids": []string{}}, {"album_ids": []string{"media-album"}, "tag_ids": []any{nil}},
		{"album_ids": []string{"media-album"}, "tag_ids": []string{}, "actor_user_id": "other"},
		{"album_ids": []string{"media-album"}, "tag_ids": []string{}, "source_config_version": 7},
	} {
		expectCode(t, f.admin.request("POST", path, body, "", store.NewID()), 400, "INVALID_JSON")
	}
	for _, body := range []map[string]any{
		{}, {"album_ids": []string{}, "tag_ids": []string{}}, {"album_ids": []string{"media-album"}},
		{"album_ids": []string{"media-album", "media-album"}, "tag_ids": []string{}},
		{"album_ids": []string{"media-album"}, "tag_ids": []string{"unknown"}},
		{"album_ids": make([]string, 101), "tag_ids": []string{}},
	} {
		expectCode(t, f.admin.request("POST", path, body, "", store.NewID()), 422, "VALIDATION_FAILED")
	}
	for _, raw := range []string{`{"album_ids":["media-album"],"tag_ids":[],"tag_ids":["tag-one"]}`, `{"album_ids":["media-album"],"tag_ids":[],"tag_IDs":[]}`} {
		r := httptest.NewRequest("POST", path, strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("X-CSRF-Token", f.admin.csrf)
		r.Header.Set("Idempotency-Key", store.NewID())
		r.AddCookie(f.admin.cookie)
		w := httptest.NewRecorder()
		f.http.ServeHTTP(w, r)
		expectCode(t, w, 400, "INVALID_JSON")
	}
	expectCode(t, f.admin.request("POST", path, valid, "", ""), 422, "VALIDATION_FAILED")
	expectCode(t, f.admin.request("POST", path, map[string]any{"album_ids": []string{"missing"}, "tag_ids": []string{}}, "", store.NewID()), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`CREATE TRIGGER reject_batch_receipt BEFORE INSERT ON mutation_receipts BEGIN SELECT RAISE(ABORT,'/private/SQL/batch'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("POST", path, valid, "", store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(problemContent(t, failed), "/private") || strings.Contains(problemContent(t, failed), "SQL") {
		t.Fatal("batch error leaked private SQL", failed.Body.String())
	}
	var count int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs)+(SELECT COUNT(*) FROM album_tag_batches)+(SELECT COUNT(*) FROM album_tag_batch_targets)+(SELECT COUNT(*) FROM job_resources)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("late receipt left partially accepted job", count, err)
	}
}

func TestTagBatchHTTPTargetKeysetsBoundToAdminScopeAccountAndJob(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	ids := make([]string, 100)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range ids {
		ids[index] = fmt.Sprintf("batch-http-%03d", index)
		if _, err := tx.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES(?,'media-library','media-source','Paged',0,0)`, ids[index]); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	job := decodeResponse[jobs.Job](t, f.admin.request("POST", "/api/v2/admin/batches/tags", map[string]any{"album_ids": ids, "tag_ids": []string{}}, "", store.NewID()), 202)
	path := "/api/v2/admin/batches/" + job.ID + "/targets"
	first := decodeResponse[listPage[access.TagBatchTarget]](t, f.admin.request("GET", path, nil, "", ""), 200)
	if len(first.Items) != 30 || !first.HasMore || first.NextCursor == nil {
		t.Fatal("target page did not use bounded default", first)
	}
	last := decodeResponse[listPage[access.TagBatchTarget]](t, f.admin.request("GET", path+"?limit=100&cursor="+*first.NextCursor, nil, "", ""), 200)
	if len(last.Items) != 70 || last.HasMore || last.NextCursor != nil || last.Items[69].AlbumID != ids[99] {
		t.Fatal("target keysets duplicated/lost accepted targets", last)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2"} {
		expectCode(t, f.admin.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	expectCode(t, f.admin.request("GET", path+"?status=queued", nil, "", ""), 400, "INVALID_FILTER")
	expectCode(t, f.admin.request("GET", "/api/v2/admin/batches/other/targets?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, f.admin.request("GET", "/api/v2/admin/jobs?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	if _, err := f.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	if _, err := f.db.Exec(`UPDATE users SET scope_id='rotated-batch-scope' WHERE username='owner'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("GET", path+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	if _, err := f.db.Exec(`UPDATE users SET role='user' WHERE username='owner'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("GET", path, nil, "", ""), 403, "ADMIN_REQUIRED")
}
