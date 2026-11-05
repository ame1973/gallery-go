// Package store provides the application's local SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"modernc.org/sqlite"
)

// IsBusy identifies actual SQLite writer contention, not error-message text.
// Retrying it is never proof of a successful heartbeat, lease, or readiness check.
func IsBusy(err error) bool {
	var failure *sqlite.Error
	return errors.As(err, &failure) && (failure.Code()&255 == 5 || failure.Code()&255 == 6)
}

// Open accepts a filesystem path, never a caller-supplied SQLite URI or SQL pragma.
// The database must reside on a local filesystem with working WAL locking.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("database path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("database must be a regular file, not a symlink")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat database: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	params := url.Values{
		"_pragma": {"busy_timeout(5000)", "foreign_keys(1)", "journal_mode(WAL)", "synchronous(FULL)"},
		"_txlock": {"immediate"},
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: params.Encode()}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	return db, nil
}
