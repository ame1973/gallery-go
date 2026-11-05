package app

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/health"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

func TestStandaloneWorkerRequiresMigrationButNotAssetsOrListener(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "worker: {embedded: false, required: false, concurrency: 1}\nsources: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Worker(t.Context(), c); err == nil {
		t.Fatal("worker silently migrated schema")
	}
	if _, err := WorkerReadiness(t.Context(), c); err == nil {
		t.Fatal("worker readiness silently migrated schema")
	}
	db, err := store.Open(t.Context(), c.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := WorkerReadiness(t.Context(), c); !errors.Is(err, health.ErrWorkerUnavailable) {
		t.Fatal("metadata-only config bypassed explicit worker check", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	c.Listen = listener.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { defer close(done); done <- Worker(ctx, c) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker cleanup did not finish")
		}
	}()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM workers WHERE role='worker' AND state='running' AND slots=1`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		select {
		case err := <-done:
			t.Fatal("worker stopped before registering", err)
		case <-timeout.C:
			t.Fatal("worker did not register")
		case <-tick.C:
		}
	}
	state, err := WorkerReadiness(t.Context(), c)
	if err != nil || state.Status != "ok" || state.Worker != "ok" {
		t.Fatal(state, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not shut down")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workers WHERE state='stopped'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("worker did not journal shutdown", count, err)
	}
	if _, err := WorkerReadiness(t.Context(), c); !errors.Is(err, health.ErrWorkerUnavailable) {
		t.Fatal("stopped worker remained ready", err)
	}
	if _, err := os.Stat(c.WebRoot); !os.IsNotExist(err) {
		t.Fatal("worker touched web asset directory", err)
	}
}

func TestEmbeddedWorkerFailureStopsHTTPBeforeClosingStorage(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "sources: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	c.Listen = "127.0.0.1:0"
	if err := os.MkdirAll(c.WebRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.WebRoot, "index.html"), []byte("<!doctype html><title>isolated</title>"), 0o400); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), c.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_worker_start BEFORE INSERT ON workers BEGIN SELECT RAISE(ABORT,'injected worker registration failure'); END`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := Serve(ctx, c, "test"); err == nil || !strings.Contains(err.Error(), "injected worker registration failure") || ctx.Err() != nil {
		t.Fatal("embedded failure left HTTP running or hid its cause", err)
	}
}

func TestScanRuntimeRejectsInvalidResourcePolicies(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, "sources: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), c.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, field := range []string{"auth", "worker", "required", "scanner", "inspection", "scheduler"} {
		invalid := c
		switch field {
		case "auth":
			invalid.Auth.Idle = 0
		case "worker":
			invalid.Worker.Concurrency = 0
		case "required":
			invalid.Worker.Required = false
		case "scanner":
			invalid.Scanner.MaxFiles = 0
		case "inspection":
			invalid.Inspection.MaxPixels = 0
		case "scheduler":
			invalid.Scheduler.Interval = 0
		}
		if _, err := newScanRuntime(db, invalid); err == nil {
			t.Fatal("invalid runtime policy accepted", field)
		}
	}
}
