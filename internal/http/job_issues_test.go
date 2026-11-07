package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gallerygo/internal/jobs"
)

func TestJobIssueHTTPPagesBindAccountScopeAndJobWithoutPrivateDetails(t *testing.T) {
	db, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	_, job := queuedHTTPJob(t, db, "issue-pages")
	_, other := queuedHTTPJob(t, db, "other-issue-pages")
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 105 {
		id := fmt.Sprintf("issue-%03d", index)
		path, code := fmt.Sprintf("相簿/photo-%03d.jpg", index), "INVALID_MEDIA"
		if index == 104 {
			path, code = "/private/nas/marker", "/private/worker/stderr"
		}
		if _, err := tx.Exec(`INSERT INTO job_issues VALUES(?,?,1,'removed-source',7,'historical-generation',?,?,123)`, id, job.ID, path, code); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO job_issues VALUES('other-job-issue',?,1,'other-source',1,'','.', 'SOURCE_UNAVAILABLE',456)`, other.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/admin/jobs/" + job.ID + "/issues"
	response := admin.request("GET", path, nil, "", "")
	page := decodeResponse[listPage[jobs.Issue]](t, response, 200)
	if len(page.Items) != 30 || !page.HasMore || page.NextCursor == nil || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("issue page was not bounded/private", page, response.Header())
	}
	first := decodeResponse[listPage[jobs.Issue]](t, admin.request("GET", path+"?limit=100", nil, "", ""), 200)
	if len(first.Items) != 100 || !first.HasMore || first.NextCursor == nil {
		t.Fatal("maximum issue page lost its keyset boundary", first)
	}
	lastResponse := admin.request("GET", path+"?limit=100&cursor="+*first.NextCursor, nil, "", "")
	last := decodeResponse[listPage[jobs.Issue]](t, lastResponse, 200)
	if len(last.Items) != 5 || last.HasMore || last.NextCursor != nil || last.Items[4].RelativePath != nil || last.Items[4].ErrorCode != "PROCESSING_FAILED" {
		t.Fatal("issue cursor lost rows or exposed unsafe detail", last)
	}
	seen := map[string]bool{}
	for _, item := range append(first.Items, last.Items...) {
		if seen[item.ID] || item.ID == "other-job-issue" || item.SourceID != "removed-source" || item.SourceConfigVersion != 7 || item.GenerationID == nil || *item.GenerationID != "historical-generation" || item.Message == "" {
			t.Fatal("issue pages duplicated, mixed or reinterpreted historical context", item)
		}
		seen[item.ID] = true
	}
	for _, secret := range []string{"/private", "stderr", "private_root", "payload_json", "lease_owner", "lease_expires_at", "fingerprint", "dedupe_token"} {
		if strings.Contains(lastResponse.Body.String(), secret) {
			t.Fatal("private job/worker/filesystem data leaked", secret, lastResponse.Body.String())
		}
	}
	for _, route := range []string{"/api/v2/admin/jobs/" + other.ID + "/issues", "/api/v2/admin/jobs/" + job.ID + "/attempts", "/api/v2/admin/jobs"} {
		expectCode(t, admin.request("GET", route+"?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "cursor=x&cursor=y"} {
		expectCode(t, admin.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	expectCode(t, admin.request("GET", path+"?status=failed", nil, "", ""), 400, "INVALID_FILTER")
	expectCode(t, admin.request("GET", path+"?cursor=invalid", nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, admin.request("GET", "/api/v2/admin/jobs/missing/issues", nil, "", ""), 404, "NOT_FOUND")
	empty := decodeResponse[listPage[jobs.Issue]](t, admin.request("GET", "/api/v2/admin/jobs/"+other.ID+"/issues", nil, "", ""), 200)
	if len(empty.Items) != 1 || empty.Items[0].GenerationID != nil || empty.Items[0].RelativePath == nil || *empty.Items[0].RelativePath != "." {
		t.Fatal("generationless source failure was replaced with invented context", empty)
	}
	if _, err := db.Exec(`UPDATE users SET scope_id='rotated-issue-scope',version=version+1 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", path+"?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
}

func TestJobIssueHTTPAdministrativeBoundaryAndReadFailures(t *testing.T) {
	db, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	viewerUser := createHTTPUser(t, admin, "issue-reader", "user")
	viewer := accountClient(t, h, viewerUser.Username, testPassword)
	_, job := queuedHTTPJob(t, db, "issue-auth")
	path := "/api/v2/admin/jobs/" + job.ID + "/issues"
	for _, client := range []struct {
		value  adminClient
		status int
		code   string
	}{
		{adminClient{handler: h}, 401, "UNAUTHENTICATED"},
		{adminClient{handler: h, cookie: &http.Cookie{Name: sessionCookie, Value: strings.Repeat("x", 43)}}, 401, "UNAUTHENTICATED"},
		{viewer, 403, "ADMIN_REQUIRED"},
	} {
		for _, route := range []string{path, "/api/v2/admin/jobs/missing/issues"} {
			expectCode(t, client.value.request("GET", route, nil, "", ""), client.status, client.code)
		}
	}
	page := decodeResponse[listPage[jobs.Issue]](t, admin.request("GET", path, nil, "", ""), 200)
	if page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil || page.HasMore {
		t.Fatal("job without diagnostics did not return an explicit empty page", page)
	}
	if _, err := db.Exec(`DROP TABLE job_issues`); err != nil {
		t.Fatal(err)
	}
	failed := admin.request("GET", path, nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(problemContent(t, failed), "job_issues") || strings.Contains(problemContent(t, failed), "SELECT") {
		t.Fatal("diagnostic SQL failure leaked internal detail", failed.Body.String())
	}
	if _, err := db.Exec(`UPDATE users SET active=0 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
}
