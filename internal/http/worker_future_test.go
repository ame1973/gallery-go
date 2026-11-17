package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/health"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
)

func TestReadinessHTTPRejectsFutureWorkerHeartbeat(t *testing.T) {
	db, _ := newTestServer(t)
	a, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	sources := library.NewSources(db, a, queue, nil)
	handler := New(db, fstest.MapFS{"index.html": {Data: []byte("health")}}, "test", a, testOrigin,
		media.New(db, a, "", nil), jobs.NewAdmin(queue, a), sources, testScans(t, db, a),
		health.New(db, queue, sources, true), testTagBatches(t, db, a))
	check := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/health/ready", nil))
		return w
	}
	if err := queue.RegisterWorker(t.Context(), "future-http-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE workers SET last_seen_at=? WHERE id='future-http-worker'`, time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	expectCode(t, check(), http.StatusServiceUnavailable, "NOT_READY")
	if err := queue.RegisterWorker(t.Context(), "current-http-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	if result := check(); result.Code != http.StatusOK {
		t.Fatal("fresh worker should restore readiness", result.Code, result.Body.String())
	}
	if err := queue.StopWorker(t.Context(), "current-http-worker"); err != nil {
		t.Fatal(err)
	}
	expectCode(t, check(), http.StatusServiceUnavailable, "NOT_READY")
}
