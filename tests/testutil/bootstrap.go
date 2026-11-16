// Package testutil provides isolated, production-strength test database setup.
package testutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

// BootstrapImage creates an in-memory copy of a new SQLite database after real
// migrations and bootstrap. It never includes a session or an application key.
// Callers must copy the returned bytes to a distinct temporary file per fixture.
func BootstrapImage(ctx context.Context, username, password string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "gallerygo-test-bootstrap-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "template.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := store.Migrate(ctx, db, migrations.Files); err != nil {
		return nil, err
	}
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		return nil, err
	}
	if _, err := service.Bootstrap(ctx, username, password); err != nil {
		return nil, err
	}
	var sessions, keys int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM sessions), (SELECT COUNT(*) FROM favorite_delete_confirmation_secret)`).Scan(&sessions, &keys); err != nil {
		return nil, err
	}
	if sessions != 0 || keys != 0 {
		return nil, errors.New("bootstrap template contains session or application key")
	}
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return nil, err
	}
	if check != "ok" {
		return nil, errors.New("bootstrap template failed SQLite quick_check")
	}
	var busy, frames, checkpointed int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed); err != nil {
		return nil, err
	}
	if busy != 0 || frames != 0 || checkpointed != 0 {
		return nil, fmt.Errorf("bootstrap checkpoint incomplete: %d %d %d", busy, frames, checkpointed)
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		return nil, errors.New("bootstrap template retained a nonempty WAL")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return os.ReadFile(path)
}

// CopyImage writes only to a new absolute path; it never overwrites a database.
func CopyImage(path string, image []byte) (err error) {
	if !filepath.IsAbs(path) || len(image) == 0 {
		return errors.New("an absolute path and nonempty image are required")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	written, err := file.Write(image)
	if err == nil && written != len(image) {
		return io.ErrShortWrite
	}
	return err
}
