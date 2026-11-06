package httpapi

import (
	"fmt"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func TestTagBatchHTTPResultsReflectRealApplicationAndPreserveHistoricalVersions(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	const enqueue = "/api/v2/admin/batches/tags"
	job := decodeResponse[jobs.Job](t, f.admin.request("POST", enqueue, map[string]any{"album_ids": []string{"media-album", "other-tag-album"}, "tag_ids": []string{"tag-one"}}, "", store.NewID()), 202)
	path := "/api/v2/admin/batches/" + job.ID + "/results"
	empty := decodeResponse[listPage[access.AlbumBatchResult]](t, f.admin.request("GET", path, nil, "", ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.HasMore || empty.NextCursor != nil {
		t.Fatal("queued intent invented applied target results", empty)
	}
	a, err := auth.New(f.db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	q, err := jobs.New(f.db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := q.Claim(t.Context(), "http-result-worker", []string{"batch_tags"})
	if err != nil {
		t.Fatal(err)
	}
	if err := access.NewAlbumBatches(f.db, a, q).Process(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	response := f.admin.request("GET", path, nil, "", "")
	results := decodeResponse[listPage[access.AlbumBatchResult]](t, response, 200)
	if len(results.Items) != 2 || results.HasMore || results.NextCursor != nil || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("committed results missing/private caching broken", results, response.Header())
	}
	for _, result := range results.Items {
		if result.Outcome != "applied" || result.Attempt != 1 || result.AlbumVersion == nil || *result.AlbumVersion != 2 || result.ErrorCode != nil || result.Message != nil {
			t.Fatal("actual processing result was not exact", result)
		}
		actual := decodeResponse[access.AlbumTagSet](t, f.admin.request("GET", "/api/v2/admin/albums/"+result.AlbumID+"/tags", nil, "", ""), 200)
		if actual.Version != 2 || len(actual.TagIDs) != 1 || actual.TagIDs[0] != "tag-one" {
			t.Fatal("applied result lacks actual metadata", actual)
		}
	}
	guest := adminClient{handler: f.http}
	expectCode(t, guest.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
	expectCode(t, f.reader.request("GET", path, nil, "", ""), 403, "ADMIN_REQUIRED")
	if _, err := f.db.Exec(`UPDATE sources SET is_active=0 WHERE id='media-source'`); err != nil {
		t.Fatal(err)
	}
	history := decodeResponse[listPage[access.AlbumBatchResult]](t, f.admin.request("GET", path, nil, "", ""), 200)
	if len(history.Items) != 2 || history.Items[0].AlbumVersion == nil || *history.Items[0].AlbumVersion != 2 {
		t.Fatal("source state erased historical results", history)
	}
	for _, secret := range []string{f.source, "root_path", "relative_path", "lease_owner", "tag_ids_json", "payload_json"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("result DTO leaked private internals", secret)
		}
	}
	expectCode(t, f.admin.request("GET", "/api/v2/admin/batches/missing/results", nil, "", ""), 404, "NOT_FOUND")
}

func TestTagBatchHTTPResultKeysetsBindJobAccountAndScope(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	ids := make([]string, 100)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range ids {
		ids[index] = fmt.Sprintf("result-page-%03d", index)
		if _, err := tx.Exec(`INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES(?,'media-library','Paged',0,0)`, ids[index]); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	job := decodeResponse[jobs.Job](t, f.admin.request("POST", "/api/v2/admin/batches/tags", map[string]any{"album_ids": ids, "tag_ids": []string{}}, "", store.NewID()), 202)
	a, err := auth.New(f.db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	q, err := jobs.New(f.db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := q.Claim(t.Context(), "result-page-worker", []string{"batch_tags"})
	if err != nil {
		t.Fatal(err)
	}
	if err := access.NewAlbumBatches(f.db, a, q).Process(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/admin/batches/" + job.ID + "/results"
	first := decodeResponse[listPage[access.AlbumBatchResult]](t, f.admin.request("GET", path, nil, "", ""), 200)
	if len(first.Items) != 30 || !first.HasMore || first.NextCursor == nil {
		t.Fatal("result page lacked bounded defaults", first)
	}
	last := decodeResponse[listPage[access.AlbumBatchResult]](t, f.admin.request("GET", path+"?limit=100&cursor="+*first.NextCursor, nil, "", ""), 200)
	if len(last.Items) != 70 || last.HasMore || last.NextCursor != nil || last.Items[0].ID <= first.Items[29].ID {
		t.Fatal("result keysets duplicated/lost committed rows", last)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2"} {
		expectCode(t, f.admin.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	expectCode(t, f.admin.request("GET", path+"?attempt=1", nil, "", ""), 400, "INVALID_FILTER")
	for _, route := range []string{"/api/v2/admin/batches/other/results", "/api/v2/admin/batches/" + job.ID + "/targets", "/api/v2/admin/jobs"} {
		expectCode(t, f.admin.request("GET", route+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	}
	if _, err := f.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	if _, err := f.db.Exec(`UPDATE users SET scope_id='new-result-scope' WHERE username='owner'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("GET", path+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
}

func TestTagBatchHTTPUnknownStoredFailureCodeNeverBecomesRawDiagnostics(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	job := decodeResponse[jobs.Job](t, f.admin.request("POST", "/api/v2/admin/batches/tags", map[string]any{"album_ids": []string{"media-album"}, "tag_ids": []string{}}, "", store.NewID()), 202)
	q, err := jobs.New(f.db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := q.Claim(t.Context(), "stored-failure-fixture", []string{"batch_tags"})
	if err != nil {
		t.Fatal(err)
	}
	// A corrupt stored-code fixture tests projection only, not target application.
	if _, err := f.db.Exec(`INSERT INTO album_tag_batch_runs(job_id,attempt,target_count,created_at) VALUES(?,?,1,0)`, job.ID, lease.Job.Attempts); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO album_tag_batch_results(id,job_id,attempt,album_id,outcome,error_code,created_at) VALUES(?,?,?,'media-album','failed','/private/SQL/secret',0)`, store.NewID(), job.ID, lease.Job.Attempts); err != nil {
		t.Fatal(err)
	}
	response := f.admin.request("GET", "/api/v2/admin/batches/"+job.ID+"/results", nil, "", "")
	results := decodeResponse[listPage[access.AlbumBatchResult]](t, response, 200)
	if len(results.Items) != 1 || results.Items[0].ErrorCode == nil || *results.Items[0].ErrorCode != "PROCESSING_FAILED" || results.Items[0].Message == nil || strings.Contains(response.Body.String(), "/private") || strings.Contains(response.Body.String(), "SQL") {
		t.Fatal("stored diagnostic was exposed verbatim", response.Body.String())
	}
}
