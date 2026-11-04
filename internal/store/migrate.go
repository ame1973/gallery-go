package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"time"
)

var migrationName = regexp.MustCompile(`^\d{4}_[a-z0-9_]+\.sql$`)

const migrationLedger = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at INTEGER NOT NULL
)`

type migration struct {
	name, checksum, sql string
}

func loadMigrations(files fs.FS) ([]migration, error) {
	names, err := fs.Glob(files, "*.sql")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("no SQL migrations found")
	}
	sort.Strings(names)
	out := make([]migration, 0, len(names))
	for i, name := range names {
		if !migrationName.MatchString(name) || (i > 0 && names[i-1][:4] == name[:4]) {
			return nil, fmt.Errorf("invalid or duplicate migration number: %s", name)
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("empty migration: %s", name)
		}
		out = append(out, migration{name, fmt.Sprintf("%x", sha256.Sum256(data)), string(data)})
	}
	return out, nil
}

// Migrate validates history before applying pending SQL and its checksums in one
// transaction. A failed statement leaves both the schema and ledger unchanged.
func Migrate(ctx context.Context, db *sql.DB, files fs.FS) error {
	all, err := loadMigrations(files)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, migrationLedger); err != nil {
		return err
	}
	applied, err := checkHistory(ctx, tx, all)
	if err != nil {
		return err
	}
	for _, m := range all[applied:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, checksum, applied_at) VALUES (?, ?, ?)`, m.name, m.checksum, time.Now().UTC().UnixMilli()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// VerifySchema fails startup on unapplied, altered, missing, or future migrations.
func VerifySchema(ctx context.Context, db *sql.DB, files fs.FS) error {
	all, err := loadMigrations(files)
	if err != nil {
		return err
	}
	applied, err := checkHistory(ctx, db, all)
	if err != nil {
		return fmt.Errorf("verify database (run gallerygo migrate first): %w", err)
	}
	if applied != len(all) {
		return errors.New("database has pending migrations; run gallerygo migrate first")
	}
	return nil
}

// Both sql.DB and sql.Tx support this narrow read operation.
type historyReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func checkHistory(ctx context.Context, db historyReader, all []migration) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			return 0, err
		}
		if count >= len(all) || all[count].name != name || all[count].checksum != checksum {
			return 0, fmt.Errorf("migration history mismatch at %s; restore matching application/schema", name)
		}
		count++
	}
	return count, rows.Err()
}
