package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	httpapi "gallerygo/internal/http"
	"gallerygo/internal/jobs"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

// Serve requires an already migrated database. It never silently upgrades a
// deployed schema or writes to a configured source root.
func Serve(ctx context.Context, c Config, version string) error {
	db, err := store.Open(ctx, c.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := store.VerifySchema(ctx, db, migrations.Files); err != nil {
		return err
	}
	assets, err := os.OpenRoot(c.WebRoot)
	if err != nil {
		return err
	}
	defer assets.Close()
	if f, err := assets.Open("index.html"); err != nil {
		return err
	} else {
		f.Close()
	}
	runtime, err := newScanRuntime(db, c)
	if err != nil {
		return err
	}
	content := media.New(db, runtime.authentication, c.CacheRoot, runtime.roots)
	work := jobs.NewAdmin(runtime.queue, runtime.authentication)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           httpapi.New(db, assets.FS(), version, runtime.authentication, c.BaseURL, content, work, runtime.sources, runtime.scans),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 32 << 10,
		BaseContext:    func(net.Listener) context.Context { return runCtx },
	}
	slog.Info("HTTP server listening", "address", listener.Addr().String(), "version", version)
	if !c.Worker.Embedded {
		return serveUntilCancelled(runCtx, server, listener)
	}
	// Either component's failure stops the other before storage is closed. Workers
	// stop claiming, cancel active handlers, and settle only still-owned attempts.
	done := make(chan error, 2)
	go func() { done <- runtime.run(runCtx, "serve") }()
	go func() { done <- serveUntilCancelled(runCtx, server, listener) }()
	err = <-done
	cancel()
	return errors.Join(err, <-done)
}

func serveUntilCancelled(ctx context.Context, server *http.Server, listener net.Listener) error {
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			server.Close()
			return err
		}
		return nil
	}
}
