package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

const sourceTestPassword = "a private source administration test phrase"

type sourceFixture struct {
	service       *Sources
	db            *sql.DB
	auth          *auth.Service
	access        *access.Service
	queue         *jobs.Queue
	owner         auth.Session
	first, second access.Library
	roots         map[string]media.SourceRoot
}

func sourceSetup(t *testing.T) sourceFixture {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "sources.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Bootstrap(t.Context(), "owner", sourceTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := a.Login(t.Context(), "owner", sourceTestPassword, "192.0.2.1", "source tests", "")
	if err != nil {
		t.Fatal(err)
	}
	policies := access.New(db, a)
	first, err := policies.CreateLibrary(t.Context(), owner, "First", true, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	second, err := policies.CreateLibrary(t.Context(), owner, "Second", true, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	roots := map[string]media.SourceRoot{}
	for _, name := range []string{"photos", "replacement"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".mounted"), []byte("isolated read-only marker"), 0o400); err != nil {
			t.Fatal(err)
		}
		roots[name] = media.SourceRoot{Path: root, Marker: ".mounted"}
	}
	roots["unmounted"] = media.SourceRoot{Path: filepath.Join(t.TempDir(), "not-mounted"), Marker: ".mounted"}
	return sourceFixture{NewSources(db, a, queue, roots), db, a, policies, queue, owner, first, second, roots}
}

func (f sourceFixture) create(t *testing.T, name string) Source {
	t.Helper()
	input := DefaultSourceSettings()
	input.LibraryID, input.RootName = f.first.ID, name
	source, err := f.service.Create(t.Context(), f.owner, input, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func sourceError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v want=%v", err, want)
	}
}
func sourcePointer[T any](value T) *T { return &value }

func (f sourceFixture) enqueue(t *testing.T, source Source) jobs.Job {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	job, created, err := f.queue.EnqueueTx(t.Context(), tx, jobs.Input{Kind: "scan", ResourceKey: "source:" + source.ID, Dedupe: "scan:" + source.ID, Payload: json.RawMessage(`{}`), ActorID: f.owner.User.ID})
	if err != nil || !created {
		t.Fatal("enqueue", created, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return job
}

func (f sourceFixture) reader(t *testing.T, name, libraryID string) auth.Session {
	t.Helper()
	user, err := f.auth.CreateUser(t.Context(), f.owner, auth.NewUser{Username: name, Password: sourceTestPassword, Role: "user", Active: true}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	group, err := f.access.CreateGroup(t.Context(), f.owner, name, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.SetUserGroups(t.Context(), f.owner, user.ID, user.Version, []string{group.ID}); err != nil {
		t.Fatal(err)
	}
	library, err := f.access.AdminLibrary(t.Context(), f.owner, libraryID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.SetLibraryPolicy(t.Context(), f.owner, libraryID, library.Version, []string{group.ID}); err != nil {
		t.Fatal(err)
	}
	reader, err := f.auth.Login(t.Context(), name, sourceTestPassword, "192.0.2.2", "source reader", "")
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func TestSourceCreationReceiptsPrivateRootsNoopsAndEmptyDeletion(t *testing.T) {
	f := sourceSetup(t)
	input := DefaultSourceSettings()
	input.LibraryID, input.RootName = f.first.ID, "photos"
	key := store.NewID()
	source, err := f.service.Create(t.Context(), f.owner, input, key)
	if err != nil || !source.Configured || source.State != "unknown" || source.ConfigVersion != 1 {
		t.Fatal(source, err)
	}
	replay, err := f.service.Create(t.Context(), f.owner, input, key)
	if err != nil || replay.ID != source.ID || replay.Version != source.Version {
		t.Fatal("duplicate source creation", replay, err)
	}
	_, err = f.service.Create(t.Context(), f.owner, input, store.NewID())
	sourceError(t, err, ErrRootInUse)
	input.RootName = "replacement"
	_, err = f.service.Create(t.Context(), f.owner, input, key)
	sourceError(t, err, store.ErrIdempotencyReuse)
	encoded, _ := json.Marshal(source)
	if strings.Contains(string(encoded), f.roots["photos"].Path) || strings.Contains(string(encoded), "root_config_sha256") || strings.Contains(fmt.Sprint(source), source.rootPath) {
		t.Fatal("private source configuration leaked")
	}
	// Mutating the caller's registry after construction cannot redirect the service.
	f.roots["photos"] = f.roots["replacement"]
	current, err := f.service.Get(t.Context(), f.owner, source.ID)
	if err != nil || !current.Configured {
		t.Fatal("registry was not frozen", current, err)
	}
	job := f.enqueue(t, source)
	before, err := f.auth.Authenticate(t.Context(), f.owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	noop, err := f.service.Update(t.Context(), f.owner, source.ID, source.Version, SourcePatch{Active: sourcePointer(true)})
	if err != nil || noop.Version != source.Version || noop.ConfigVersion != source.ConfigVersion {
		t.Fatal("no-op changed source version", noop, err)
	}
	after, err := f.auth.Authenticate(t.Context(), f.owner.Token)
	if err != nil || before.User.ScopeID != after.User.ScopeID {
		t.Fatal("no-op rotated scope", err)
	}
	pending, err := f.queue.Get(t.Context(), job.ID)
	if err != nil || pending.Status != "queued" {
		t.Fatal("no-op cancelled work", pending, err)
	}
	sourceError(t, f.service.DeleteEmpty(t.Context(), f.owner, source.ID, 99), auth.ErrConflict)
	if err := f.service.DeleteEmpty(t.Context(), f.owner, source.ID, source.Version); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.Get(t.Context(), f.owner, source.ID)
	sourceError(t, err, sql.ErrNoRows)
	pending, err = f.queue.Get(t.Context(), job.ID)
	if err != nil || pending.Status != "cancelled" {
		t.Fatal("deleted configuration left queued work", pending, err)
	}
}

func TestSourceReassignmentInvalidatesScopesLeasesAndOldAlbumVisibility(t *testing.T) {
	f := sourceSetup(t)
	source := f.create(t, "photos")
	oldReader := f.reader(t, "old-reader", f.first.ID)
	newReader := f.reader(t, "new-reader", f.second.ID)
	for _, query := range []string{
		`INSERT INTO albums(id,library_id,source_id,source_name,display_name,relative_path,created_at,updated_at) VALUES('retained-album',?,?,'Original','Display override','folder',0,0)`,
	} {
		if _, err := f.db.Exec(query, f.first.ID, source.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,sha256,updated_at) VALUES('retained-entry',?,'folder/item.png',1,0,?,0)`, source.ID, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO media(id,album_id,source_entry_id,kind,file_name,created_at,updated_at) VALUES('retained-media','retained-album','retained-entry','image','item.png',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorites(user_id,album_id,created_at) VALUES(?,'retained-album',0)`, oldReader.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.Album(t.Context(), oldReader, "retained-album"); err != nil {
		t.Fatal(err)
	}
	sourceError(t, f.service.DeleteEmpty(t.Context(), f.owner, source.ID, source.Version), ErrSourceNotEmpty)
	job := f.enqueue(t, source)
	lease, err := f.queue.Claim(t.Context(), "source-worker", []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := f.service.Update(t.Context(), f.owner, source.ID, source.Version, SourcePatch{LibraryID: &f.second.ID})
	if err != nil || updated.ConfigVersion != 2 || updated.Version != 2 || updated.State != "unknown" {
		t.Fatal(updated, err)
	}
	for _, reader := range []auth.Session{oldReader, newReader, f.owner} {
		fresh, err := f.auth.Authenticate(t.Context(), reader.Token)
		if err != nil {
			t.Fatal("source policy revoked valid login", err)
		}
		if fresh.User.ScopeID == reader.User.ScopeID {
			t.Fatal("affected library scope was not rotated")
		}
		_, err = f.access.Album(t.Context(), fresh, "retained-album")
		sourceError(t, err, sql.ErrNoRows)
	}
	pending, err := f.queue.Get(t.Context(), job.ID)
	if err != nil || !pending.CancelRequested || pending.Status != "running" {
		t.Fatal("stale scan was not cancelled", pending, err)
	}
	sourceError(t, f.queue.Heartbeat(t.Context(), lease), jobs.ErrCancelled)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = f.service.RequireConfigTx(t.Context(), tx, source.ID, source.ConfigVersion)
	sourceError(t, err, ErrSourceChanged)
	tx.Rollback()
	var retained int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM favorites f JOIN albums a ON a.id=f.album_id JOIN media m ON m.album_id=a.id JOIN source_entries e ON e.id=m.source_entry_id WHERE a.display_name='Display override' AND a.library_id=? AND e.availability='present'`, f.first.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("configuration change rewrote the old inventory or interaction", retained, err)
	}
}

func TestSourceDisableHidesMetadataAndRegistryChangesFenceOtherProcesses(t *testing.T) {
	f := sourceSetup(t)
	source := f.create(t, "photos")
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES('active-album',?,?,'Album',0,0)`, f.first.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.Album(t.Context(), f.owner, "active-album"); err != nil {
		t.Fatal(err)
	}
	inactive, err := f.service.Update(t.Context(), f.owner, source.ID, source.Version, SourcePatch{Active: sourcePointer(false)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.access.Album(t.Context(), f.owner, "active-album")
	sourceError(t, err, sql.ErrNoRows)
	active, err := f.service.Update(t.Context(), f.owner, source.ID, inactive.Version, SourcePatch{Active: sourcePointer(true)})
	if err != nil {
		t.Fatal(err)
	}
	newRoots := map[string]media.SourceRoot{"photos": {Path: source.rootPath, Marker: ".new-marker"}}
	other := NewSources(f.db, f.auth, f.queue, newRoots)
	mismatched, err := other.Get(t.Context(), f.owner, source.ID)
	if err != nil || mismatched.Configured {
		t.Fatal("changed mount marker was accepted without a new epoch", mismatched, err)
	}
	pinned, err := other.Update(t.Context(), f.owner, source.ID, active.Version, SourcePatch{RootName: sourcePointer("photos")})
	if err != nil || !pinned.Configured || pinned.ConfigVersion <= active.ConfigVersion {
		t.Fatal(pinned, err)
	}
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, _, err = f.service.RequireConfigTx(t.Context(), tx, source.ID, pinned.ConfigVersion)
	sourceError(t, err, ErrSourceChanged)
}

func TestSourceMutationOutboxAndReceiptFailuresRestoreAllState(t *testing.T) {
	f := sourceSetup(t)
	source := f.create(t, "photos")
	job := f.enqueue(t, source)
	before, err := f.auth.Authenticate(t.Context(), f.owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_source_event BEFORE INSERT ON outbox_events WHEN (SELECT kind FROM change_log WHERE seq=NEW.change_seq)='source' BEGIN SELECT RAISE(ABORT,'injected source event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Update(t.Context(), f.owner, source.ID, source.Version, SourcePatch{MinimumFiles: sourcePointer(int64(10))}); err == nil {
		t.Fatal("source committed without outbox")
	}
	current, err := f.service.Get(t.Context(), f.owner, source.ID)
	if err != nil || current.Version != source.Version || current.ConfigVersion != source.ConfigVersion || current.MinimumFiles != 0 {
		t.Fatal("partial source update", current, err)
	}
	after, err := f.auth.Authenticate(t.Context(), f.owner.Token)
	if err != nil || after.User.ScopeID != before.User.ScopeID {
		t.Fatal("scope revoke survived failed source update", err)
	}
	pending, err := f.queue.Get(t.Context(), job.ID)
	if err != nil || pending.CancelRequested || pending.Status != "queued" {
		t.Fatal("job cancellation survived failed source update", pending, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_source_event; CREATE TRIGGER fail_source_receipt BEFORE INSERT ON mutation_receipts BEGIN SELECT RAISE(ABORT,'injected source receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Check(t.Context(), f.owner, source.ID, source.Version, store.NewID()); err == nil {
		t.Fatal("reachability committed without receipt")
	}
	current, err = f.service.Get(t.Context(), f.owner, source.ID)
	if err != nil || current.State != "unknown" || current.Version != source.Version || current.LastCheckedAt != nil {
		t.Fatal("partial health mutation", current, err)
	}
	input := DefaultSourceSettings()
	input.LibraryID, input.RootName = f.first.ID, "replacement"
	if _, err := f.service.Create(t.Context(), f.owner, input, store.NewID()); err == nil {
		t.Fatal("source created without receipt")
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&count); err != nil || count != 1 {
		t.Fatal("partial source creation", count, err)
	}
}

func TestSourceOperationsRecheckRoleAndDisabledSession(t *testing.T) {
	f := sourceSetup(t)
	source := f.create(t, "photos")
	input := DefaultSourceSettings()
	input.LibraryID, input.RootName = f.first.ID, "replacement"
	checks := []func() error{
		func() error { _, e := f.service.Get(t.Context(), f.owner, source.ID); return e },
		func() error { _, e := f.service.List(t.Context(), f.owner, "", 30); return e },
		func() error { _, e := f.service.Roots(t.Context(), f.owner, "", 30); return e },
		func() error { _, e := f.service.Create(t.Context(), f.owner, input, store.NewID()); return e },
		func() error {
			_, e := f.service.Update(t.Context(), f.owner, source.ID, source.Version, SourcePatch{Active: sourcePointer(false)})
			return e
		},
		func() error {
			_, e := f.service.Check(t.Context(), f.owner, source.ID, source.Version, store.NewID())
			return e
		},
		func() error { return f.service.DeleteEmpty(t.Context(), f.owner, source.ID, source.Version) },
	}
	if _, err := f.db.Exec(`UPDATE users SET role='user' WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		sourceError(t, check(), auth.ErrForbidden)
	}
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		sourceError(t, check(), auth.ErrUnauthenticated)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.service.Get(ctx, f.owner, source.ID)
	sourceError(t, err, context.Canceled)
}
