package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "private ?# library.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateFreshAndRepeat(t *testing.T) {
	db := testDB(t)
	if err := VerifySchema(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("unmigrated database accepted")
	}
	for range 2 {
		if err := Migrate(t.Context(), db, migrations.Files); err != nil {
			t.Fatal(err)
		}
		if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	all, err := loadMigrations(migrations.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil || count != len(all) {
		t.Fatalf("ledger=%d err=%v", count, err)
	}
	if _, err := db.Exec(`INSERT INTO album_search(album_id,title) VALUES ('test','mountain sunrise')`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_search WHERE album_search MATCH 'sunrise'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("FTS5=%d err=%v", count, err)
	}
	if _, err := db.Exec(`INSERT INTO ratings(user_id, album_id, rating, updated_at) VALUES ('missing','missing',3,0)`); err == nil {
		t.Fatal("foreign key violation accepted")
	}
}

func TestEveryConnectionHasPragmas(t *testing.T) {
	db := testDB(t)
	var conns []*sql.Conn
	for range 4 {
		conn, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		t.Cleanup(func() { conn.Close() })
	}
	for _, conn := range conns {
		for pragma, want := range map[string]string{"foreign_keys": "1", "busy_timeout": "5000", "journal_mode": "wal", "synchronous": "2"} {
			var got string
			if err := conn.QueryRowContext(t.Context(), "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
				t.Fatalf("%s=%s want %s: %v", pragma, got, want, err)
			}
		}
	}
}

func TestMigrationRollbackAndHistory(t *testing.T) {
	base := fstest.MapFS{"0001_first.sql": {Data: []byte(`CREATE TABLE example(id INTEGER PRIMARY KEY);`)}}
	t.Run("failure rolls back entire batch", func(t *testing.T) {
		db := testDB(t)
		broken := fstest.MapFS{"0001_first.sql": base["0001_first.sql"], "0002_bad.sql": {Data: []byte(`CREATE TABLE partial(id INTEGER); NOT VALID SQL;`)}}
		if err := Migrate(t.Context(), db, broken); err == nil {
			t.Fatal("broken SQL accepted")
		}
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial schema persisted: %d %v", count, err)
		}
	})
	t.Run("checksum drift", func(t *testing.T) {
		db := testDB(t)
		if err := Migrate(t.Context(), db, base); err != nil {
			t.Fatal(err)
		}
		changed := fstest.MapFS{"0001_first.sql": {Data: []byte(`CREATE TABLE example(id TEXT);`)}}
		if err := Migrate(t.Context(), db, changed); err == nil {
			t.Fatal("checksum drift accepted")
		}
	})
	t.Run("future and missing migration", func(t *testing.T) {
		db := testDB(t)
		two := fstest.MapFS{"0001_first.sql": base["0001_first.sql"], "0002_second.sql": {Data: []byte(`CREATE TABLE second(id INTEGER);`)}}
		if err := Migrate(t.Context(), db, two); err != nil {
			t.Fatal(err)
		}
		if err := VerifySchema(t.Context(), db, base); err == nil {
			t.Fatal("future schema accepted")
		}
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version='0001_first.sql'`); err != nil {
			t.Fatal(err)
		}
		if err := Migrate(t.Context(), db, two); err == nil {
			t.Fatal("missing history accepted")
		}
	})
	t.Run("pending migration blocks serving", func(t *testing.T) {
		db := testDB(t)
		if err := Migrate(t.Context(), db, base); err != nil {
			t.Fatal(err)
		}
		two := fstest.MapFS{"0001_first.sql": base["0001_first.sql"], "0002_second.sql": {Data: []byte(`CREATE TABLE second(id INTEGER);`)}}
		if err := VerifySchema(t.Context(), db, two); err == nil || !strings.Contains(err.Error(), "pending") {
			t.Fatalf("expected pending: %v", err)
		}
	})
}

func TestInvalidMigrationSets(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"empty": {}, "bad name": {"bad.sql": {Data: []byte("SELECT 1;")}},
		"empty file": {"0001_empty.sql": {Data: []byte{}}},
		"duplicate":  {"0001_a.sql": {Data: []byte("SELECT 1;")}, "0001_b.sql": {Data: []byte("SELECT 1;")}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Migrate(t.Context(), testDB(t), files); err == nil {
				t.Fatal("invalid migration set accepted")
			}
		})
	}
}

func TestConcurrentMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	a, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	for _, db := range []*sql.DB{a, b} {
		wg.Go(func() {
			if err := Migrate(t.Context(), db, migrations.Files); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := VerifySchema(t.Context(), a, migrations.Files); err != nil {
		t.Fatal(err)
	}
}

func TestOpenValidationAndCancellation(t *testing.T) {
	for _, path := range []string{"", "relative.db"} {
		if db, err := Open(t.Context(), path); err == nil {
			db.Close()
			t.Fatal("relative path accepted")
		}
	}
	dir := t.TempDir()
	if db, err := Open(t.Context(), dir); err == nil {
		db.Close()
		t.Fatal("directory accepted")
	}
	target := filepath.Join(dir, "target.db")
	if err := os.WriteFile(target, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if db, err := Open(t.Context(), link); err == nil {
		db.Close()
		t.Fatal("symlink accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if db, err := Open(ctx, filepath.Join(dir, "cancel.db")); err == nil {
		db.Close()
		t.Fatal("cancelled open succeeded")
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private permissions: %v %v", info, err)
	}
}
