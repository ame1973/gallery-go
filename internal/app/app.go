package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"gallerygo/internal/auth"
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
	authentication, err := auth.New(db, c.Auth)
	if err != nil {
		return err
	}
	roots := make(map[string]media.SourceRoot, len(c.Sources))
	for _, source := range c.Sources {
		roots[source.Name] = media.SourceRoot{Path: source.Root, Marker: source.Marker}
	}
	content := media.New(db, authentication, c.CacheRoot, roots)
	queue, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		return err
	}
	work := jobs.NewAdmin(queue, authentication)
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler:           httpapi.New(db, assets.FS(), version, authentication, c.BaseURL, content, work),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 32 << 10,
		BaseContext:    func(net.Listener) context.Context { return ctx },
	}
	slog.Info("HTTP server listening", "address", listener.Addr().String(), "version", version)
	return serveUntilCancelled(ctx, server, listener)
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
