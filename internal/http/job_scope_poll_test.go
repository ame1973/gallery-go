package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
)

// This controlled interleaving exercises the real inventory scope revocation
// transaction, not a concurrent scan or the unexplained original CLI failure.
func TestJobReadRejectsCommittedInventoryScopeTransition(t *testing.T) {
	db, router := authFixture(t)
	admin, before := ownerClient(t, router)
	queue, job := queuedHTTPJob(t, db, "scope-poll")
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	protected := (authentication{service: service}).protect(func(w http.ResponseWriter, r *http.Request) {
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := access.RevokeInventoryScopesTx(r.Context(), tx, "isolated-source", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		(jobHandler{service: jobs.NewAdmin(queue, service)}).get(w, r)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v2/admin/jobs/"+job.ID, nil)
	request.SetPathValue("job_id", job.ID)
	request.AddCookie(admin.cookie)
	request.Header.Set("X-GalleryGo-Expected-User", before.ID)
	request.Header.Set("X-GalleryGo-Expected-Scope", before.ScopeID)
	response := httptest.NewRecorder()
	response.Header().Set("X-Request-ID", "inventory-scope-test")
	protected(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("job read after committed scope change returned %d", response.Code)
	}
	expectCode(t, response, http.StatusConflict, "VERSION_CONFLICT")
	freshResponse := admin.request(http.MethodGet, "/api/v2/auth/me", nil, "", "")
	fresh := decodeResponse[auth.User](t, freshResponse, http.StatusOK)
	if fresh.ID != before.ID || fresh.ScopeID == before.ScopeID || fresh.Version <= before.Version || fresh.Role != "admin" || !fresh.Active {
		t.Fatal("the active administrator did not receive a new scope")
	}
	var revokes, outbox int
	if err := db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM change_log WHERE kind='scope' AND resource_id=? AND actor_scope=? AND operation='revoke'),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='scope' AND c.resource_id=? AND c.actor_scope=? AND c.operation='revoke')`, before.ID, before.ScopeID, before.ID, before.ScopeID).Scan(&revokes, &outbox); err != nil || revokes != 1 || outbox != 1 {
		t.Fatalf("scope transition journals: revoke=%d outbox=%d error=%v", revokes, outbox, err)
	}
	freshJob := admin.request(http.MethodGet, "/api/v2/admin/jobs/"+job.ID, nil, "", "")
	if got := responseJob(t, freshJob, http.StatusOK); got.ID != job.ID {
		t.Fatal("fresh job read returned the wrong job")
	}
}
