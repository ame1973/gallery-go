package httpapi

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"gallerygo/internal/store"
)

func metricsResponse(t *testing.T, client adminClient, want map[string]int64) {
	t.Helper()
	response := client.request(http.MethodGet, "/api/v2/admin/metrics", nil, "", "")
	result := decodeResponse[map[string]map[string]int64](t, response, http.StatusOK)
	if !reflect.DeepEqual(result, map[string]map[string]int64{"jobs": want}) {
		t.Fatalf("job metrics = %#v, want %#v", result, want)
	}
	if response.Header().Get("Cache-Control") != "private, no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("metrics response is missing private security headers", response.Header())
	}
}

func TestAdminMetricsHTTP(t *testing.T) {
	db, handler := authFixture(t)
	admin, owner := ownerClient(t, handler)
	empty := map[string]int64{"queued": 0, "running": 0, "succeeded": 0, "failed": 0, "cancelled": 0, "dead": 0}
	metricsResponse(t, admin, empty)

	readerUser := createHTTPUser(t, admin, "metrics-reader", "user")
	reader := accountClient(t, handler, readerUser.Username, testPassword)
	for _, test := range []struct {
		client adminClient
		status int
		code   string
	}{
		{adminClient{handler: handler}, 401, "UNAUTHENTICATED"},
		{adminClient{handler: handler, cookie: &http.Cookie{Name: sessionCookie, Value: strings.Repeat("x", 43)}}, 401, "UNAUTHENTICATED"},
		{reader, 403, "ADMIN_REQUIRED"},
	} {
		expectCode(t, test.client.request("GET", "/api/v2/admin/metrics", nil, "", ""), test.status, test.code)
	}
	for _, query := range []string{"?status=queued", "?limit=1", "?%zz"} {
		expectCode(t, admin.request("GET", "/api/v2/admin/metrics"+query, nil, "", ""), 400, "INVALID_FILTER")
	}

	for i, status := range []string{"queued", "queued", "running", "succeeded", "failed", "cancelled", "dead"} {
		_, job := queuedHTTPJob(t, db, string(rune('a'+i)))
		if _, err := db.Exec(`UPDATE jobs SET status=? WHERE id=?`, status, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	var changesBefore, outboxBefore, auditBefore int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`).Scan(&changesBefore, &outboxBefore, &auditBefore); err != nil {
		t.Fatal(err)
	}
	metricsResponse(t, admin, map[string]int64{"queued": 2, "running": 1, "succeeded": 1, "failed": 1, "cancelled": 1, "dead": 1})
	var changesAfter, outboxAfter, auditAfter int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`).Scan(&changesAfter, &outboxAfter, &auditAfter); err != nil {
		t.Fatal(err)
	}
	if changesAfter != changesBefore || outboxAfter != outboxBefore || auditAfter != auditBefore {
		t.Fatal("reading metrics changed the business event history")
	}

	// A corrupt stored status must fail closed without exposing its value or SQL.
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), `PRAGMA ignore_check_constraints=ON`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `UPDATE jobs SET status='private-status-canary' WHERE id=(SELECT id FROM jobs LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `PRAGMA ignore_check_constraints=OFF`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	response := admin.request("GET", "/api/v2/admin/metrics", nil, "", "")
	expectCode(t, response, 500, "INTERNAL_ERROR")
	if strings.Contains(response.Body.String(), "private-status-canary") || strings.Contains(response.Body.String(), "SELECT") {
		t.Fatal("metrics exposed a private database diagnostic")
	}
	if _, err := db.Exec(`UPDATE users SET role='user' WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", "/api/v2/admin/metrics", nil, "", ""), 403, "ADMIN_REQUIRED")
	if _, err := db.Exec(`UPDATE users SET active=0 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", "/api/v2/admin/metrics", nil, "", ""), 401, "UNAUTHENTICATED")
}

func TestAdminMetricsTracksRealJobTransitions(t *testing.T) {
	db, handler := authFixture(t)
	admin, _ := ownerClient(t, handler)
	queue, job := queuedHTTPJob(t, db, "transitions")
	counts := map[string]int64{"queued": 1, "running": 0, "succeeded": 0, "failed": 0, "cancelled": 0, "dead": 0}
	metricsResponse(t, admin, counts)
	lease, err := queue.Claim(t.Context(), "metrics-test-worker", []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	counts["queued"], counts["running"] = 0, 1
	metricsResponse(t, admin, counts)
	if err := queue.Fail(t.Context(), lease, "SOURCE_UNAVAILABLE", false); err != nil {
		t.Fatal(err)
	}
	counts["running"], counts["failed"] = 0, 1
	metricsResponse(t, admin, counts)
	path := "/api/v2/admin/jobs/" + job.ID
	current := admin.request("GET", path, nil, "", "")
	responseJob(t, current, 200)
	responseJob(t, admin.request("POST", path+"/retry", nil, current.Header().Get("ETag"), store.NewID()), 202)
	counts["failed"], counts["queued"] = 0, 1
	metricsResponse(t, admin, counts)
}
