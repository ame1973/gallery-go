package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	"gallerygo/internal/auth"
	"gallerygo/internal/health"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/scanner"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

// scanRuntime keeps embedded and standalone execution on the same queue, frozen
// root registry, handler, and resource limits. It does not probe source files.
// No service publishes inventory merely because the process starts successfully.
type scanRuntime struct {
	authentication *auth.Service
	queue          *jobs.Queue
	sources        *library.Sources
	scans          *scanner.Scans
	handler        jobs.Handler
	roots          map[string]media.SourceRoot
	schedule       scanner.SchedulePolicy
}

func newScanRuntime(db *sql.DB, c Config) (*scanRuntime, error) {
	if err := c.Scheduler.Validate(); err != nil {
		return nil, err
	}
	if err := c.Worker.Validate(); err != nil {
		return nil, err
	}
	authentication, err := auth.New(db, c.Auth)
	if err != nil {
		return nil, err
	}
	queue, err := jobs.New(db, c.Worker.Policy)
	if err != nil {
		return nil, err
	}
	roots := make(map[string]media.SourceRoot, len(c.Sources))
	for _, source := range c.Sources {
		roots[source.Name] = media.SourceRoot{Path: source.Root, Marker: source.Marker}
	}
	sources := library.NewSources(db, authentication, queue, roots)
	scans := scanner.NewScans(db, authentication, queue)
	inventory, err := scanner.NewInventory(db, queue, sources, c.Scanner)
	if err != nil {
		return nil, err
	}
	handler, err := scans.Handler(inventory, c.Inspection)
	if err != nil {
		return nil, err
	}
	return &scanRuntime{authentication, queue, sources, scans, handler, roots, c.Scheduler}, nil
}

func (r *scanRuntime) run(ctx context.Context, role string) error {
	consume := func(ctx context.Context) error {
		return r.queue.Run(ctx, role, map[string]jobs.Handler{"scan": r.handler})
	}
	if !r.schedule.Enabled {
		return consume(ctx)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- consume(runCtx) }()
	go func() { done <- r.scans.RunSchedule(runCtx, r.sources, r.schedule) }()
	err := <-done
	cancel()
	return errors.Join(err, <-done)
}

// WorkerReadiness probes shared ingestion capacity without an HTTP listener. It
// requires a fresh committed worker heartbeat even for metadata-only server config.
// This is a dependency check, not identity/liveness proof for a particular process.
func WorkerReadiness(ctx context.Context, c Config) (health.Readiness, error) {
	db, err := store.Open(ctx, c.Database)
	if err != nil {
		return health.Readiness{}, err
	}
	defer db.Close()
	if err := store.VerifySchema(ctx, db, migrations.Files); err != nil {
		return health.Readiness{}, err
	}
	runtime, err := newScanRuntime(db, c)
	if err != nil {
		return health.Readiness{}, err
	}
	return health.New(db, runtime.queue, runtime.sources, true).Check(ctx)
}

// Worker consumes accepted scan intents without a listener or web asset dependency.
// It never migrates implicitly, creates source roots/markers, or scans without a job.
func Worker(ctx context.Context, c Config) error {
	db, err := store.Open(ctx, c.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.VerifySchema(ctx, db, migrations.Files); err != nil {
		return err
	}
	runtime, err := newScanRuntime(db, c)
	if err != nil {
		return err
	}
	slog.Info("scan worker starting", "slots", c.Worker.Concurrency)
	return runtime.run(ctx, "worker")
}
