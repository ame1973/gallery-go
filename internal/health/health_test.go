package health

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

func healthFixture(t *testing.T) (*Service, *jobs.Queue, media.SourceRoot) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	root := media.SourceRoot{Path: filepath.Join(t.TempDir(), "not-mounted"), Marker: ".private-marker"}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at,updated_at) VALUES('health-library','Private library',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources(id,library_id,name,root_path,root_config_sha256,mapping_json,is_active,state,last_checked_at,created_at) VALUES('source-000','health-library','private-root',?,?,'{}',1,'healthy',?,0)`, root.Path, root.Signature(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	sources := library.NewSources(db, nil, queue, map[string]media.SourceRoot{"private-root": root})
	return New(db, queue, sources, true), queue, root
}

func TestReadinessRequiresFreshCommittedWorkerAndKeepsSourceOutagesDegraded(t *testing.T) {
	service, queue, root := healthFixture(t)
	if _, err := service.Check(t.Context()); !errors.Is(err, ErrWorkerUnavailable) {
		t.Fatal("missing worker became ready", err)
	}
	if err := queue.RegisterWorker(t.Context(), "health-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	state, err := service.Check(t.Context())
	if err != nil || state.Status != "ok" || state.Database != "ok" || state.Worker != "ok" || state.Sources != "ok" {
		t.Fatal(state, err)
	}
	if _, err := os.Stat(root.Path); !os.IsNotExist(err) {
		t.Fatal("readiness touched absent source", err)
	}
	if _, err := service.db.Exec(`UPDATE sources SET state='unavailable'`); err != nil {
		t.Fatal(err)
	}
	state, err = service.Check(t.Context())
	if err != nil || state.Status != "degraded" || state.Database != "ok" || state.Worker != "ok" || state.Sources != "degraded" {
		t.Fatal("source outage hid metadata availability", state, err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{root.Path, root.Marker, "private-root", "health-library", "source-000", "last_checked_at", "root_config_sha256"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("private source state leaked", string(data))
		}
	}
	if _, err := service.db.Exec(`UPDATE workers SET last_seen_at=?`, time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(t.Context()); !errors.Is(err, ErrWorkerUnavailable) {
		t.Fatal("stale worker became ready", err)
	}
	if err := queue.WorkerHeartbeat(t.Context(), "health-worker"); err != nil {
		t.Fatal(err)
	}
	if err := queue.StopWorker(t.Context(), "health-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(t.Context()); !errors.Is(err, ErrWorkerUnavailable) {
		t.Fatal("stopped worker became ready", err)
	}
}

func TestSourceReadinessRejectsUnknownStaleFutureAndMismatchedObservations(t *testing.T) {
	service, _, root := healthFixture(t)
	service.requireWorker = false
	for _, change := range []string{
		`UPDATE sources SET state='unknown'`,
		`UPDATE sources SET last_checked_at=NULL`,
		fmt.Sprintf(`UPDATE sources SET last_checked_at=%d`, time.Now().Add(-121*time.Second).UnixMilli()),
		fmt.Sprintf(`UPDATE sources SET last_checked_at=%d`, time.Now().Add(time.Hour).UnixMilli()),
		`UPDATE sources SET root_config_sha256='` + strings.Repeat("0", 64) + `'`,
	} {
		if _, err := service.db.Exec(`UPDATE sources SET state='healthy',last_checked_at=?,root_config_sha256=?`, time.Now().UnixMilli(), root.Signature()); err != nil {
			t.Fatal(err)
		}
		if _, err := service.db.Exec(change); err != nil {
			t.Fatal(err)
		}
		state, err := service.Check(t.Context())
		if err != nil || state.Status != "degraded" || state.Worker != "optional" {
			t.Fatal("unsafe observation treated as current", change, state, err)
		}
	}
	if _, err := service.db.Exec(`UPDATE sources SET is_active=0`); err != nil {
		t.Fatal(err)
	}
	state, err := service.Check(t.Context())
	if err != nil || state.Status != "ok" || state.Sources != "ok" {
		t.Fatal("inactive source was a readiness dependency", state, err)
	}
	var changes, gen int
	if err := service.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log)+(SELECT COUNT(*) FROM audit_events),(SELECT scan_generation FROM sources)`).Scan(&changes, &gen); err != nil || changes != 0 || gen != 0 {
		t.Fatal("readiness mutated health/index evidence", changes, gen, err)
	}
}

func TestSourceReadinessPagesPastOneHundredRows(t *testing.T) {
	service, queue, root := healthFixture(t)
	service.requireWorker = false
	roots := map[string]media.SourceRoot{"private-root": root}
	for index := 1; index < 202; index++ {
		name := fmt.Sprintf("root-%03d", index)
		current := media.SourceRoot{Path: filepath.Join(t.TempDir(), "not-mounted"), Marker: ".marker"}
		roots[name] = current
		if _, err := service.db.Exec(`INSERT INTO sources(id,library_id,name,root_path,root_config_sha256,mapping_json,is_active,state,last_checked_at,created_at) VALUES(?,'health-library',?,?,?,'{}',1,'healthy',?,0)`, fmt.Sprintf("source-%03d", index), name, current.Path, current.Signature(), time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	service.sources = library.NewSources(service.db, nil, queue, roots)
	state, err := service.Check(t.Context())
	if err != nil || state.Status != "ok" {
		t.Fatal(state, err)
	}
	if _, err := service.db.Exec(`UPDATE sources SET state='error' WHERE id='source-201'`); err != nil {
		t.Fatal(err)
	}
	state, err = service.Check(t.Context())
	if err != nil || state.Status != "degraded" {
		t.Fatal("late source page was omitted", state, err)
	}
}

func TestReadinessDependencyFaultsNeverBecomeHealthyOrOptionalFallback(t *testing.T) {
	service, queue, _ := healthFixture(t)
	var missing *Service
	if _, err := missing.Check(t.Context()); !errors.Is(err, ErrDependencies) {
		t.Fatal(err)
	}
	if _, err := New(nil, nil, nil, false).Check(t.Context()); !errors.Is(err, ErrDependencies) {
		t.Fatal(err)
	}
	if _, err := New(service.db, nil, service.sources, true).Check(t.Context()); !errors.Is(err, ErrDependencies) {
		t.Fatal("required worker silently made optional", err)
	}
	if err := queue.RegisterWorker(t.Context(), "health-worker", "serve", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := service.db.Exec(`DROP TABLE workers`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(t.Context()); !errors.Is(err, ErrDependencies) {
		t.Fatal("worker query failure hidden", err)
	}
	service.requireWorker = false
	if _, err := service.db.Exec(`UPDATE sources SET mapping_json='{"unknown":true}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(t.Context()); !errors.Is(err, ErrDependencies) {
		t.Fatal("stored source corruption hidden", err)
	}
	if err := service.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(t.Context()); !errors.Is(err, ErrDependencies) {
		t.Fatal("database failure became success", err)
	}
}
