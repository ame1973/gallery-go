// Package health reports bounded, privacy-safe readiness from local committed
// dependency evidence. It never touches source files or promises codec readiness.
package health

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
)

var (
	ErrWorkerUnavailable = errors.New("no required scan worker has a fresh committed heartbeat")
	ErrDependencies      = errors.New("service dependency checks could not be completed")
)

type Readiness struct {
	Status   string `json:"status"`
	Database string `json:"database"`
	Worker   string `json:"worker"`
	Sources  string `json:"sources"`
}

type Service struct {
	db            *sql.DB
	queue         *jobs.Queue
	sources       *library.Sources
	requireWorker bool
}

func New(db *sql.DB, queue *jobs.Queue, sources *library.Sources, requireWorker bool) *Service {
	return &Service{db, queue, sources, requireWorker}
}

func (s *Service) Check(ctx context.Context) (Readiness, error) {
	if s == nil || s.db == nil || s.sources == nil || (s.requireWorker && s.queue == nil) {
		return Readiness{}, ErrDependencies
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.db.PingContext(ctx); err != nil {
		return Readiness{}, fmt.Errorf("%w: %w", ErrDependencies, err)
	}
	state := Readiness{Status: "ok", Database: "ok", Worker: "optional", Sources: "ok"}
	if s.requireWorker {
		available, err := s.queue.Available(ctx)
		if err != nil {
			return Readiness{}, fmt.Errorf("%w: %w", ErrDependencies, err)
		}
		if !available {
			return Readiness{}, ErrWorkerUnavailable
		}
		state.Worker = "ok"
	}
	ready, err := s.sources.Ready(ctx)
	if err != nil {
		return Readiness{}, fmt.Errorf("%w: %w", ErrDependencies, err)
	}
	if !ready {
		state.Status, state.Sources = "degraded", "degraded"
	}
	return state, nil
}
