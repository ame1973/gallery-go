package testutil

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
	"gallerygo/migrations"

	"golang.org/x/crypto/bcrypt"
)

const testPassword = "a private fixture test passphrase"

func openImage(t *testing.T, image []byte) (*auth.Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := CopyImage(path, image); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var mode, hash, username, role string
	var users, sessions, keys, installations, audits, changes int
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatal("fixture is not using WAL", mode, err)
	}
	if err := db.QueryRow(`SELECT username,role,password_hash FROM users`).Scan(&username, &role, &hash); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM users), (SELECT COUNT(*) FROM sessions),
		(SELECT COUNT(*) FROM favorite_delete_confirmation_secret), (SELECT COUNT(*) FROM installation),
		(SELECT COUNT(*) FROM audit_events WHERE event_type='admin_bootstrapped'),
		(SELECT COUNT(*) FROM change_log WHERE kind='user')`).Scan(&users, &sessions, &keys, &installations, &audits, &changes); err != nil {
		t.Fatal(err)
	}
	if username != "owner" || role != "admin" || users != 1 || sessions != 0 || keys != 0 || installations != 1 || audits != 1 || changes != 1 {
		t.Fatal("image lost bootstrap or contains a session/key", username, role, users, sessions, keys, installations, audits, changes)
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil || cost != 12 {
		t.Fatal("fixture does not use the production bcrypt cost", cost, err)
	}
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return service, path
}

func TestBootstrapImageCopiesRemainIsolated(t *testing.T) {
	image, err := BootstrapImage(t.Context(), "owner", testPassword)
	if err != nil || len(image) == 0 {
		t.Fatal("real bootstrap image", len(image), err)
	}
	first, firstPath := openImage(t, image)
	second, secondPath := openImage(t, image)
	if firstPath == secondPath {
		t.Fatal("fixtures share a database path")
	}
	one, err := first.Login(t.Context(), "owner", testPassword, "192.0.2.1", "first fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	two, err := second.Login(t.Context(), "owner", testPassword, "192.0.2.2", "second fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if one.Token == two.Token || one.ID == two.ID {
		t.Fatal("independent logins reused a session or token")
	}
	if _, err := second.Authenticate(t.Context(), one.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("session from another temporary database was accepted", err)
	}
	if _, err := first.Authenticate(t.Context(), two.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("reverse cross-database session was accepted", err)
	}
	if _, err := first.Bootstrap(t.Context(), "another", testPassword); !errors.Is(err, auth.ErrBootstrapped) {
		t.Fatal("bootstrapped latch was not preserved", err)
	}
	if err := first.Revoke(t.Context(), one, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Authenticate(t.Context(), two.Token); err != nil {
		t.Fatal("a separate fixture was changed by revocation", err)
	}
}

func TestCopyImageRejectsExistingAndUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.db")
	if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path  string
		image []byte
	}{{path, []byte("replacement")}, {"relative.db", []byte("image")}, {filepath.Join(dir, "empty.db"), nil}} {
		if err := CopyImage(test.path, test.image); err == nil {
			t.Fatal("unsafe target or empty image was accepted", test.path)
		}
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "unchanged" {
		t.Fatal("existing target was overwritten", string(content), err)
	}
}

func TestConcurrentImageCopiesDoNotShareState(t *testing.T) {
	image, err := BootstrapImage(t.Context(), "owner", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	const count = 8
	paths := make([]string, count)
	for i := range paths {
		paths[i] = filepath.Join(t.TempDir(), "copy.db")
	}
	var wg sync.WaitGroup
	for _, path := range paths {
		wg.Go(func() {
			if err := CopyImage(path, image); err != nil {
				t.Error(err)
				return
			}
			db, err := store.Open(t.Context(), path)
			if err != nil {
				t.Error(err)
				return
			}
			defer db.Close()
			if _, err := db.Exec(`INSERT INTO permission_groups(id,name,created_at) VALUES('private-copy','Private',0)`); err != nil {
				t.Error(err)
				return
			}
			var count int
			if err := db.QueryRow(`SELECT COUNT(*) FROM permission_groups`).Scan(&count); err != nil || count != 1 {
				t.Error("copy shares state", count, err)
			}
		})
	}
	wg.Wait()
}

func TestBootstrapImageCleansUpOnSuccessAndFailure(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		name     string
		ctx      context.Context
		password string
		wantErr  bool
	}{
		{"success", t.Context(), testPassword, false},
		{"invalid password", t.Context(), "short", true},
		{"cancelled context", cancelled, testPassword, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := BootstrapImage(test.ctx, "owner", test.password)
			if (err != nil) != test.wantErr {
				t.Fatal("unexpected bootstrap result", err)
			}
			entries, err := os.ReadDir(temp)
			if err != nil || len(entries) != 0 {
				t.Fatal("temporary bootstrap files remain", entries, err)
			}
		})
	}
}
