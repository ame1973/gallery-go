package httpapi

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func queuedHTTPJob(t *testing.T, db *sql.DB, resource string) (*jobs.Queue, jobs.Job) {
	t.Helper()
	q, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	job, created, err := q.EnqueueTx(t.Context(), tx, jobs.Input{Kind: "scan", ResourceKey: "source:" + resource, Dedupe: "scan:" + resource, Payload: json.RawMessage(`{"private_root":"/not-for-export","source_id":"opaque"}`)})
	if err != nil || !created {
		t.Fatal("enqueue fixture", created, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return q, job
}

func responseJob(t *testing.T, w *httptest.ResponseRecorder, status int) jobs.Job {
	t.Helper()
	job := decodeResponse[jobs.Job](t, w, status)
	if job.ID == "" || w.Header().Get("ETag") != resourceETag("job", job.ID, job.Version) || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("missing job identity/version/privacy", w.Header())
	}
	for _, secret := range []string{"/not-for-export", "private_root", "payload", "lease_owner", "lease_expires_at", "dedupe", "worker_id"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private job capability leaked", secret)
		}
	}
	return job
}

func TestJobsHTTPControlsPreserveReceiptsCountsAndAttemptHistory(t *testing.T) {
	db, h := authFixture(t)
	admin, _ := ownerClient(t, h)
	q, job := queuedHTTPJob(t, db, "lifecycle")
	path := "/api/v2/admin/jobs/" + job.ID
	first := admin.request("GET", path, nil, "", "")
	responseJob(t, first, 200)
	firstTag := first.Header().Get("ETag")
	cancelKey := store.NewID()
	cancelled := responseJob(t, admin.request("POST", path+"/cancel", nil, firstTag, cancelKey), 200)
	if cancelled.Status != "cancelled" {
		t.Fatal("queued cancellation was not terminal", cancelled)
	}
	retryKey := store.NewID()
	retryTag := resourceETag("job", job.ID, cancelled.Version)
	retried := responseJob(t, admin.request("POST", path+"/retry", nil, retryTag, retryKey), 202)
	if retried.Status != "queued" || retried.CancelRequested {
		t.Fatal(retried)
	}
	responseJob(t, admin.request("POST", path+"/cancel", nil, firstTag, cancelKey), 200)
	replayed := responseJob(t, admin.request("POST", path+"/retry", nil, retryTag, retryKey), 202)
	if replayed.Version != retried.Version {
		t.Fatal("retry replay repeated a mutation")
	}
	expectCode(t, admin.request("POST", path+"/retry", nil, firstTag, cancelKey), 409, "IDEMPOTENCY_CONFLICT")
	lease, err := q.Claim(t.Context(), "fixture-worker", []string{"scan"})
	if err != nil {
		t.Fatal("old cancel replay cancelled newer work", err)
	}
	if _, err := q.Progress(t.Context(), lease, jobs.Counts{Total: 2, Done: 1, Succeeded: 1}, true); err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(t.Context(), lease, "SOURCE_UNAVAILABLE", false); err != nil {
		t.Fatal(err)
	}
	failed := responseJob(t, admin.request("GET", path, nil, "", ""), 200)
	if failed.Status != "failed" || failed.Succeeded != 1 || failed.Cancelled != 1 || failed.Done != 2 {
		t.Fatal("failure invented successful work", failed)
	}
	responseJob(t, admin.request("POST", path+"/retry", nil, resourceETag("job", job.ID, failed.Version), store.NewID()), 202)
	lease, err = q.Claim(t.Context(), "fixture-worker-two", []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Progress(t.Context(), lease, jobs.Counts{Total: 3, Done: 3, Succeeded: 2, Cancelled: 1}, true); err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	finished := responseJob(t, admin.request("GET", path, nil, "", ""), 200)
	if finished.Status != "succeeded" || finished.Attempts != 2 || finished.Cancelled != 1 || finished.Succeeded != 2 {
		t.Fatal("retry history disappeared", finished)
	}
	page := decodeResponse[listPage[jobs.Attempt]](t, admin.request("GET", path+"/attempts?limit=1", nil, "", ""), 200)
	if len(page.Items) != 1 || page.Items[0].Number != 1 || page.Items[0].Status != "failed" || page.Items[0].Cancelled != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatal("attempt page", page)
	}
	last := decodeResponse[listPage[jobs.Attempt]](t, admin.request("GET", path+"/attempts?limit=1&cursor="+*page.NextCursor, nil, "", ""), 200)
	if len(last.Items) != 1 || last.Items[0].Number != 2 || last.Items[0].Status != "succeeded" || last.Items[0].Succeeded != 1 || last.HasMore || last.NextCursor != nil {
		t.Fatal("attempt-local counters/page", last)
	}
	expectCode(t, admin.request("POST", path+"/retry", nil, resourceETag("job", job.ID, finished.Version), store.NewID()), 409, "JOB_STATE_CONFLICT")
}

func TestJobFiltersCursorsAndAdministrativeAuthorization(t *testing.T) {
	db, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	viewerUser := createHTTPUser(t, admin, "job-reader", "user")
	viewer := accountClient(t, h, viewerUser.Username, testPassword)
	_, first := queuedHTTPJob(t, db, "first")
	_, second := queuedHTTPJob(t, db, "second")
	path := "/api/v2/admin/jobs/" + first.ID
	for _, client := range []struct {
		value  adminClient
		status int
		code   string
	}{
		{adminClient{handler: h}, 401, "UNAUTHENTICATED"},
		{adminClient{handler: h, cookie: &http.Cookie{Name: sessionCookie, Value: strings.Repeat("x", 43)}}, 401, "UNAUTHENTICATED"},
		{viewer, 403, "ADMIN_REQUIRED"},
	} {
		for _, route := range []struct{ method, path string }{{"GET", "/api/v2/admin/jobs"}, {"GET", path}, {"GET", path + "/attempts"}, {"POST", path + "/cancel"}, {"POST", path + "/retry"}} {
			expectCode(t, client.value.request(route.method, route.path, nil, "", store.NewID()), client.status, client.code)
		}
	}
	page := decodeResponse[listPage[jobs.Job]](t, admin.request("GET", "/api/v2/admin/jobs?status=queued&limit=1", nil, "", ""), 200)
	if len(page.Items) != 1 || !page.HasMore || page.NextCursor == nil {
		t.Fatal("job page", page)
	}
	cursor := *page.NextCursor
	last := decodeResponse[listPage[jobs.Job]](t, admin.request("GET", "/api/v2/admin/jobs?limit=1&cursor="+cursor+"&status=queued", nil, "", ""), 200)
	if len(last.Items) != 1 || last.HasMore || last.Items[0].ID == page.Items[0].ID {
		t.Fatal("job cursor repeated or lost a row", last)
	}
	for _, target := range []string{"/api/v2/admin/jobs?status=running&cursor=", "/api/v2/admin/jobs?cursor=", path + "/attempts?cursor="} {
		expectCode(t, admin.request("GET", target+cursor, nil, "", ""), 400, "INVALID_CURSOR")
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "cursor=x&cursor=y"} {
		expectCode(t, admin.request("GET", "/api/v2/admin/jobs?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	expectCode(t, admin.request("GET", "/api/v2/admin/jobs?status=queued&status=running", nil, "", ""), 400, "INVALID_FILTER")
	expectCode(t, admin.request("GET", "/api/v2/admin/jobs?status=imaginary", nil, "", ""), 422, "VALIDATION_FAILED")
	expectCode(t, admin.request("GET", "/api/v2/admin/jobs/missing", nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, admin.request("GET", "/api/v2/admin/jobs/missing/attempts", nil, "", ""), 404, "NOT_FOUND")
	attemptCursor, _ := json.Marshal(pageCursor{Scope: owner.ScopeID, User: owner.ID, Collection: path + "/attempts", After: "-1"})
	expectCode(t, admin.request("GET", path+"/attempts?cursor="+base64.RawURLEncoding.EncodeToString(attemptCursor), nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, admin.request("POST", path+"/cancel", nil, resourceETag("job", second.ID, 1), store.NewID()), 400, "INVALID_PRECONDITION")
	if _, err := db.Exec(`UPDATE users SET scope_id='new-job-scope',version=version+1 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", "/api/v2/admin/jobs?status=queued&cursor="+cursor, nil, "", ""), 400, "INVALID_CURSOR")
	if _, err := db.Exec(`UPDATE users SET active=0 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
	expectCode(t, admin.request("POST", path+"/cancel", nil, resourceETag("job", first.ID, 1), store.NewID()), 401, "UNAUTHENTICATED")
}

func TestJobCommandsRequireFreshIdentityVersionOriginCSRFAndNoBody(t *testing.T) {
	db, h := authFixture(t)
	admin, _ := ownerClient(t, h)
	_, job := queuedHTTPJob(t, db, "boundaries")
	path := "/api/v2/admin/jobs/" + job.ID + "/cancel"
	tag := resourceETag("job", job.ID, job.Version)
	expectCode(t, admin.request("POST", path, nil, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
	for _, invalid := range []string{"*", "W/" + tag, tag + ", " + tag, resourceETag("user", job.ID, 1)} {
		expectCode(t, admin.request("POST", path, nil, invalid, store.NewID()), 400, "INVALID_PRECONDITION")
	}
	expectCode(t, admin.request("POST", path, nil, resourceETag("job", job.ID, 99), store.NewID()), 409, "VERSION_CONFLICT")
	expectCode(t, admin.request("POST", path, nil, tag, ""), 422, "VALIDATION_FAILED")
	expectCode(t, admin.request("POST", path, map[string]any{"lease_owner": "forged"}, tag, store.NewID()), 400, "INVALID_BODY")
	for _, test := range []struct{ origin, csrf, code string }{{"https://elsewhere.test", admin.csrf, "ORIGIN_REJECTED"}, {testOrigin, "wrong", "CSRF_REJECTED"}} {
		r := httptest.NewRequest("POST", path, nil)
		r.AddCookie(admin.cookie)
		r.Header.Set("Origin", test.origin)
		r.Header.Set("X-CSRF-Token", test.csrf)
		r.Header.Set("If-Match", tag)
		r.Header.Set("Idempotency-Key", store.NewID())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		expectCode(t, w, 403, test.code)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_job_outbox BEFORE INSERT ON outbox_events WHEN (SELECT kind FROM change_log WHERE seq=NEW.change_seq)='job' BEGIN SELECT RAISE(ABORT,'private database diagnostic'); END`); err != nil {
		t.Fatal(err)
	}
	failed := admin.request("POST", path, nil, tag, store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(failed.Body.String(), "private database diagnostic") {
		t.Fatal("SQL diagnostic leaked")
	}
	current := responseJob(t, admin.request("GET", "/api/v2/admin/jobs/"+job.ID, nil, "", ""), 200)
	if current.Version != job.Version || current.Status != "queued" || current.CancelRequested {
		t.Fatal("failed job command partially committed", current)
	}
}
