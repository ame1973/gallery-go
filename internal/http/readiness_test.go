package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/internal/auth"
	"gallerygo/internal/health"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
)

func TestRequiredWorkerReadinessFailsClosedAndMetadataOnlyIsExplicit(t *testing.T) {
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
	build := func(probe *health.Service) func() *httptest.ResponseRecorder {
		h := New(db, fstest.MapFS{"index.html": {Data: []byte("<title>health</title>")}}, "test", a, testOrigin, media.New(db, a, "", nil), jobs.NewAdmin(queue, a), sources, testScans(t, db, a), probe)
		return func() *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/health/ready?worker=optional", nil))
			return w
		}
	}
	required := build(health.New(db, queue, sources, true))
	w := required()
	assertProblem(t, w, 503)
	if strings.Contains(w.Body.String(), "health.db") {
		t.Fatal("dependency path leaked", w.Body.String())
	}
	if err := queue.RegisterWorker(t.Context(), "api-health-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	w = required()
	var state health.Readiness
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || w.Code != 200 || state.Worker != "ok" || state.Status != "ok" {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	if err := queue.StopWorker(t.Context(), "api-health-worker"); err != nil {
		t.Fatal(err)
	}
	assertProblem(t, required(), 503)
	optional := build(health.New(db, queue, sources, false))
	w = optional()
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || w.Code != 200 || state.Worker != "optional" {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	assertProblem(t, build(nil)(), 503)
}

func TestSourceReadinessDegradesWithoutDisclosingRegistryOrWritingEvidence(t *testing.T) {
	db, h := newTestServer(t)
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at,updated_at) VALUES('private-health-library','Private health',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources(id,library_id,name,root_path,mapping_json,is_active,state,created_at) VALUES('private-health-source','private-health-library','private-health-root','/not-a-real-test-nas','{}',1,'unavailable',0)`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/v2/health/ready", nil))
	var state health.Readiness
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil || w.Code != 200 || state.Status != "degraded" || state.Database != "ok" || state.Worker != "optional" || state.Sources != "degraded" {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	if w.Header().Get("Cache-Control") != "private, no-store" || strings.Contains(w.Body.String(), "private-health") || strings.Contains(w.Body.String(), "not-a-real-test-nas") {
		t.Fatal("readiness exposed private dependency data", w.Header(), w.Body.String())
	}
	var generation, events int
	if err := db.QueryRow(`SELECT scan_generation,(SELECT COUNT(*) FROM change_log)+(SELECT COUNT(*) FROM audit_events) FROM sources`).Scan(&generation, &events); err != nil || generation != 0 || events != 0 {
		t.Fatal("readiness mutated source state", generation, events, err)
	}
}
