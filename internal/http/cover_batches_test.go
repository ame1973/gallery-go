package httpapi

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func TestCoverBatchHTTPFreezeApplyClearHistoryAndNoReadyGrant(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	if _, err := f.db.Exec(`INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('cover-alt','media-album','image','alternate.jpg',0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"targets": []map[string]string{{"album_id": "other-tag-album", "cover_media_id": ""}, {"album_id": "media-album", "cover_media_id": "cover-alt"}}}
	key := store.NewID()
	response := f.admin.request("POST", "/api/v2/admin/batches/covers", body, "", key)
	job := decodeResponse[jobs.Job](t, response, 202)
	if job.Kind != "batch_covers" || job.Status != "queued" || job.TotalKnown || response.Header().Get("Location") != "/api/v2/admin/jobs/"+job.ID || response.Header().Get("ETag") != resourceETag("job", job.ID, 1) {
		t.Fatal("cover acceptance claimed execution", job, response.Header())
	}
	path := "/api/v2/admin/batches/" + job.ID + "/covers"
	targetResponse := f.admin.request("GET", path, nil, "", "")
	targets := decodeResponse[listPage[access.CoverBatchTarget]](t, targetResponse, 200)
	if len(targets.Items) != 2 || targets.Items[0].CoverMediaID != "cover-alt" || targets.Items[0].ProjectedCoverID == nil || *targets.Items[0].ProjectedCoverID != "cover-alt" || targets.Items[0].MediaVersion == nil || *targets.Items[0].MediaVersion != 1 || targets.Items[0].RevisionID != nil || targets.Items[1].ProjectedCoverID != nil {
		t.Fatal("cover snapshots lost nullable projection/media epochs", targets)
	}
	a, err := auth.New(f.db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	q, err := jobs.New(f.db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := q.Claim(t.Context(), "http-cover-worker", []string{"batch_covers"})
	if err != nil {
		t.Fatal(err)
	}
	if err := access.NewAlbumBatches(f.db, a, q).Process(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	results := decodeResponse[listPage[access.AlbumBatchResult]](t, f.admin.request("GET", "/api/v2/admin/batches/"+job.ID+"/results", nil, "", ""), 200)
	if len(results.Items) != 2 || results.Items[0].Outcome != "applied" || results.Items[0].AlbumVersion == nil || *results.Items[0].AlbumVersion != 2 || results.Items[1].Outcome != "unchanged" {
		t.Fatal("cover result history not real application", results)
	}
	metadata := decodeResponse[access.AlbumMetadata](t, f.admin.request("GET", "/api/v2/admin/albums/media-album", nil, "", ""), 200)
	if metadata.Version != 2 || metadata.CoverMediaID == nil || *metadata.CoverMediaID != "cover-alt" || metadata.CoverOverrideID == nil || *metadata.CoverOverrideID != "cover-alt" {
		t.Fatal("batch failed to apply cover preference", metadata)
	}
	variants := f.admin.request("GET", "/api/v2/media/cover-alt/variants", nil, "", "")
	if variants.Code != 200 || !strings.Contains(variants.Body.String(), `"items":[]`) {
		t.Fatal("cover preference fabricated READY media", variants.Code, variants.Body.String())
	}
	replay := f.admin.request("POST", "/api/v2/admin/batches/covers", body, "", key)
	if old := decodeResponse[jobs.Job](t, replay, 202); old.ID != job.ID || old.Status != "queued" || replay.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("cover receipt lost historical acceptance", old)
	}
	history := decodeResponse[listPage[access.CoverBatchTarget]](t, f.admin.request("GET", path, nil, "", ""), 200)
	if !reflect.DeepEqual(history, targets) {
		t.Fatal("processing rewrote cover target snapshot", history, targets)
	}
	for _, secret := range []string{f.source, "root_path", "relative_path", "lease_owner", "payload_json", ".marker"} {
		if strings.Contains(response.Body.String()+targetResponse.Body.String(), secret) {
			t.Fatal("private cover metadata leaked", secret)
		}
	}
	guest := adminClient{handler: f.http}
	expectCode(t, guest.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
	expectCode(t, f.reader.request("GET", path, nil, "", ""), 403, "ADMIN_REQUIRED")
	expectCode(t, f.reader.request("POST", "/api/v2/admin/batches/covers", body, "", store.NewID()), 403, "ADMIN_REQUIRED")
}

func TestCoverBatchHTTPStrictNestedBodiesAndSanitizedReceiptRollback(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	const path = "/api/v2/admin/batches/covers"
	for _, body := range []map[string]any{
		{"targets": nil}, {"targets": []map[string]any{{"album_id": "media-album", "cover_media_id": nil}}},
		{"targets": []map[string]any{{"album_id": "media-album", "Cover_Media_ID": "protected"}}},
		{"targets": []map[string]any{{"album_id": "media-album", "cover_media_id": "protected", "source_id": "private"}}},
		{"targets": []map[string]any{{"album_id": "media-album", "cover_media_id": "protected", "version": 1}}},
	} {
		expectCode(t, f.admin.request("POST", path, body, "", store.NewID()), 400, "INVALID_JSON")
	}
	tooMany := make([]map[string]string, 101)
	for index := range tooMany {
		tooMany[index] = map[string]string{"album_id": "media-album", "cover_media_id": ""}
	}
	for _, body := range []map[string]any{
		{}, {"targets": []map[string]string{}}, {"targets": []map[string]string{{"album_id": "media-album"}}},
		{"targets": []map[string]string{{"album_id": "media-album", "cover_media_id": "protected"}, {"album_id": "media-album", "cover_media_id": ""}}},
		{"targets": tooMany},
	} {
		expectCode(t, f.admin.request("POST", path, body, "", store.NewID()), 422, "VALIDATION_FAILED")
	}
	valid := map[string]any{"targets": []map[string]string{{"album_id": "media-album", "cover_media_id": "protected"}}}
	expectCode(t, f.admin.request("POST", path, valid, "", ""), 422, "VALIDATION_FAILED")
	expectCode(t, requestAuth(f.http, "POST", path, `{"targets":[{"album_id":"media-album","cover_media_id":"protected"}]}`, testOrigin, "", f.admin.cookie), 403, "CSRF_REJECTED")
	if _, err := f.db.Exec(`CREATE TRIGGER reject_cover_receipt BEFORE INSERT ON mutation_receipts BEGIN SELECT RAISE(ABORT,'/private/SQL/cover'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("POST", path, valid, "", store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(problemContent(t, failed), "/private") || strings.Contains(problemContent(t, failed), "SQL") {
		t.Fatal("cover error exposed private details", failed.Body.String())
	}
	var rows int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_cover_batches)+(SELECT COUNT(*) FROM album_cover_batch_targets)+(SELECT COUNT(*) FROM jobs)`).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("failed receipt left accepted cover state", rows, err)
	}
}

func TestCoverBatchHTTPTargetPagesBindJobAccountAndCurrentScope(t *testing.T) {
	f := albumTagsHTTPFixture(t)
	targets := make([]map[string]string, 100)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range targets {
		id := fmt.Sprintf("cover-page-%03d", index)
		if _, err := tx.Exec(`INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES(?,'media-library','Paged',0,0)`, id); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		targets[index] = map[string]string{"album_id": id, "cover_media_id": ""}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	job := decodeResponse[jobs.Job](t, f.admin.request("POST", "/api/v2/admin/batches/covers", map[string]any{"targets": targets}, "", store.NewID()), 202)
	path := "/api/v2/admin/batches/" + job.ID + "/covers"
	first := decodeResponse[listPage[access.CoverBatchTarget]](t, f.admin.request("GET", path, nil, "", ""), 200)
	if len(first.Items) != 30 || !first.HasMore || first.NextCursor == nil {
		t.Fatal("cover target defaults not bounded", first)
	}
	last := decodeResponse[listPage[access.CoverBatchTarget]](t, f.admin.request("GET", path+"?limit=100&cursor="+*first.NextCursor, nil, "", ""), 200)
	if len(last.Items) != 70 || last.HasMore || last.NextCursor != nil || last.Items[69].AlbumID != "cover-page-099" {
		t.Fatal("cover keyset lost targets", last)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2"} {
		expectCode(t, f.admin.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	expectCode(t, f.admin.request("GET", path+"?status=queued", nil, "", ""), 400, "INVALID_FILTER")
	for _, route := range []string{"/api/v2/admin/batches/other/covers", "/api/v2/admin/batches/" + job.ID + "/results", "/api/v2/admin/batches/" + job.ID + "/targets"} {
		expectCode(t, f.admin.request("GET", route+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	}
	if _, err := f.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	if _, err := f.db.Exec(`UPDATE users SET scope_id='new-cover-scope' WHERE username='owner'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.admin.request("GET", path+"?cursor="+*first.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, f.admin.request("GET", "/api/v2/admin/batches/missing/covers", nil, "", ""), 404, "NOT_FOUND")
}
