package access

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

const testPassword = "a private access policy test passphrase"

func fixture(t *testing.T) (*Service, auth.Session) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "access.db"))
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
	if _, err := a.Bootstrap(t.Context(), "owner", testPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := a.Login(t.Context(), "owner", testPassword, "192.0.2.1", "access test", "")
	if err != nil {
		t.Fatal(err)
	}
	return New(db, a), owner
}

func reader(t *testing.T, s *Service, name string) auth.Session {
	t.Helper()
	// This isolated DB fixture reuses a real production-strength hash. It does not
	// weaken bcrypt or add a production provisioning endpoint; login is still real.
	id := store.NewID()
	if _, err := s.db.Exec(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at,scope_id) SELECT ?,?,password_hash,'user',0,0,? FROM users WHERE username='owner'`, id, name, "scope-"+id); err != nil {
		t.Fatal(err)
	}
	session, err := s.auth.Login(t.Context(), name, testPassword, "192.0.2.2", "reader test", "")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func group(t *testing.T, s *Service, owner auth.Session, name string) Group {
	t.Helper()
	value, err := s.CreateGroup(t.Context(), owner, name, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func library(t *testing.T, s *Service, owner auth.Session, name string) Library {
	t.Helper()
	value, err := s.CreateLibrary(t.Context(), owner, name, true, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func album(t *testing.T, s *Service, libraryID, name string) string {
	t.Helper()
	id := store.NewID()
	if _, err := s.db.Exec(`INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES(?,?,?,0,0)`, id, libraryID, name); err != nil {
		t.Fatal(err)
	}
	return id
}

func membership(t *testing.T, s *Service, owner, subject auth.Session, ids ...string) UserGroups {
	t.Helper()
	before, err := s.UserGroups(t.Context(), owner, subject.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ids == nil {
		ids = []string{}
	}
	after, err := s.SetUserGroups(t.Context(), owner, subject.User.ID, before.Version, ids)
	if err != nil {
		t.Fatal(err)
	}
	return after
}

func requireError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v want=%v", err, want)
	}
}

func requireAlbums(t *testing.T, s *Service, viewer auth.Session, expected ...string) {
	t.Helper()
	items, err := s.Albums(t.Context(), viewer, AlbumFilter{}, AlbumPosition{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
		if _, err := s.Album(t.Context(), viewer, item.ID); err != nil {
			t.Fatal("list/detail predicate mismatch", err)
		}
	}
	slices.Sort(ids)
	slices.Sort(expected)
	if !slices.Equal(ids, expected) {
		t.Fatalf("visible albums=%v want=%v", ids, expected)
	}
}

func TestLibraryIsMandatoryAlbumRestrictionIsAdditionalAndGroupsAreORed(t *testing.T) {
	s, owner := fixture(t)
	base, private, alternate := group(t, s, owner, "Library viewers"), group(t, s, owner, "Private"), group(t, s, owner, "Alternate")
	first, other := library(t, s, owner, "First"), library(t, s, owner, "Other")
	open, limited, closed, albumOnly := album(t, s, first.ID, "Open"), album(t, s, first.ID, "Limited"), album(t, s, first.ID, "Closed"), album(t, s, other.ID, "Album grant only")
	if _, err := s.SetLibraryPolicy(t.Context(), owner, first.ID, 1, []string{base.ID, alternate.ID}); err != nil {
		t.Fatal(err)
	}
	for id, groups := range map[string][]string{limited: {private.ID}, closed: {}, albumOnly: {base.ID}} {
		if _, err := s.SetAlbumPolicy(t.Context(), owner, id, 1, true, groups); err != nil {
			t.Fatal(err)
		}
	}
	alice, bob, carol, nobody := reader(t, s, "alice"), reader(t, s, "bob"), reader(t, s, "carol"), reader(t, s, "nobody")
	membership(t, s, owner, alice, base.ID)
	membership(t, s, owner, bob, private.ID)
	carolBefore := membership(t, s, owner, carol, alternate.ID, private.ID)
	requireAlbums(t, s, alice, open)
	requireAlbums(t, s, bob)
	requireAlbums(t, s, carol, open, limited)
	requireAlbums(t, s, nobody)
	requireAlbums(t, s, owner, open, limited, closed, albumOnly)
	for _, pair := range []struct {
		viewer auth.Session
		id     string
	}{{alice, limited}, {bob, limited}, {alice, albumOnly}, {nobody, open}} {
		_, err := s.Album(t.Context(), pair.viewer, pair.id)
		requireError(t, err, sql.ErrNoRows)
	}
	if err := s.DeleteGroup(t.Context(), owner, private.ID, private.Version); err != nil {
		t.Fatal(err)
	}
	policy, err := s.AlbumPolicy(t.Context(), owner, limited)
	if err != nil || !policy.Restricted || len(policy.GroupIDs) != 0 || policy.Version != 3 {
		t.Fatalf("last-group deletion widened policy: %+v %v", policy, err)
	}
	requireAlbums(t, s, carol, open)
	carolAfter, err := s.UserGroups(t.Context(), owner, carol.User.ID)
	if err != nil || carolAfter.ScopeID == carolBefore.ScopeID || carolAfter.Version != carolBefore.Version+1 {
		t.Fatalf("group deletion did not invalidate scope: %+v %v", carolAfter, err)
	}
	if _, err := s.auth.Authenticate(t.Context(), carol.Token); err != nil {
		t.Fatal("ACL epoch changes should preserve valid login sessions", err)
	}
	if _, err := s.SetAlbumPolicy(t.Context(), owner, limited, policy.Version, false, []string{}); err != nil {
		t.Fatal(err)
	}
	requireAlbums(t, s, alice, open, limited)
	if err := s.DeleteGroup(t.Context(), owner, base.ID, base.Version); err != nil {
		t.Fatal(err)
	}
	requireAlbums(t, s, alice)
	requireAlbums(t, s, carol, open, limited)
	if err := s.DeleteGroup(t.Context(), owner, alternate.ID, alternate.Version); err != nil {
		t.Fatal(err)
	}
	requireAlbums(t, s, carol)
	var grants int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM library_access WHERE library_id=?`, first.ID).Scan(&grants); err != nil || grants != 0 {
		t.Fatal("library grants were not removed safely", grants, err)
	}
}

func TestVisibilityCoversCountsCoversInactiveLibrariesAndSourceChanges(t *testing.T) {
	s, owner := fixture(t)
	g, first, second := group(t, s, owner, "Family"), library(t, s, owner, "First"), library(t, s, owner, "Second")
	if _, err := s.SetLibraryPolicy(t.Context(), owner, first.ID, 1, []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	viewer := reader(t, s, "reader")
	membership(t, s, owner, viewer, g.ID)
	id := album(t, s, first.ID, "Source title")
	queries := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO sources(id,library_id,name,root_path,mapping_json,created_at) VALUES('source-one',?,'one','/fixture/one','{}',0),('source-two',?,'two','/fixture/two','{}',0)`, []any{first.ID, second.ID}},
		{`UPDATE albums SET source_id='source-one',display_name='Visible title',cover_media_id='wrong-source' WHERE id=?`, []any{id}},
		{`INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,updated_at) VALUES('entry-one','source-one','one.jpg',1,0,0),('entry-two','source-two','two.jpg',1,0,0)`, nil},
		{`INSERT INTO media(id,album_id,source_entry_id,kind,file_name,created_at,updated_at) VALUES('visible',?,'entry-one','image','one.jpg',0,0),('wrong-source',?,'entry-two','image','two.jpg',0,0)`, []any{id, id}},
		{`INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES(?,?,5,0)`, []any{viewer.User.ID, id}},
	}
	for _, query := range queries {
		if _, err := s.db.Exec(query.query, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	value, err := s.Album(t.Context(), viewer, id)
	if err != nil || value.Title != "Visible title" || value.MediaCount != 1 || value.CoverMediaID != nil || value.RatingAvg == nil || *value.RatingAvg != 5 {
		t.Fatalf("visible aggregation: %+v %v", value, err)
	}
	if _, err := s.db.Exec(`UPDATE albums SET cover_media_id='visible' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	value, err = s.Album(t.Context(), viewer, id)
	if err != nil || value.CoverMediaID == nil || *value.CoverMediaID != "visible" {
		t.Fatalf("visible cover: %+v %v", value, err)
	}
	if _, err := s.db.Exec(`UPDATE sources SET library_id=? WHERE id='source-one'`, second.ID); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []auth.Session{viewer, owner} {
		_, err := s.Album(t.Context(), subject, id)
		requireError(t, err, sql.ErrNoRows)
	}
	if _, err := s.db.Exec(`UPDATE sources SET library_id=? WHERE id='source-one'`, first.ID); err != nil {
		t.Fatal(err)
	}
	before, err := s.AdminLibrary(t.Context(), owner, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	inactive := false
	updated, err := s.UpdateLibrary(t.Context(), owner, first.ID, before.Version, LibraryPatch{Active: &inactive})
	if err != nil || updated.Active {
		t.Fatal("library deactivation failed", err)
	}
	for _, subject := range []auth.Session{viewer, owner} {
		_, err := s.Library(t.Context(), subject, first.ID)
		requireError(t, err, sql.ErrNoRows)
		requireAlbums(t, s, subject)
	}
	if got, err := s.AdminLibrary(t.Context(), owner, first.ID); err != nil || got.Active {
		t.Fatal("inactive library is not manageable", err)
	}
	requireError(t, s.DeleteLibrary(t.Context(), owner, first.ID, updated.Version), ErrLibraryNotEmpty)
}

func TestGroupAndLibraryCRUDIdempotencyAndPreconditions(t *testing.T) {
	s, owner := fixture(t)
	key := store.NewID()
	g, err := s.CreateGroup(t.Context(), owner, " Family ", key)
	if err != nil || g.Name != "Family" || g.Version != 1 {
		t.Fatalf("create group: %+v %v", g, err)
	}
	replay, err := s.CreateGroup(t.Context(), owner, "Family", key)
	if err != nil || replay != g {
		t.Fatal("group retry was not idempotent", replay, err)
	}
	_, err = s.CreateGroup(t.Context(), owner, "Different", key)
	requireError(t, err, store.ErrIdempotencyReuse)
	_, err = s.CreateLibrary(t.Context(), owner, "Family", true, key)
	requireError(t, err, store.ErrIdempotencyReuse)
	_, err = s.CreateGroup(t.Context(), owner, "Family", store.NewID())
	requireError(t, err, ErrNameTaken)
	_, err = s.RenameGroup(t.Context(), owner, g.ID, 99, "New name")
	requireError(t, err, auth.ErrConflict)
	g, err = s.RenameGroup(t.Context(), owner, g.ID, 1, "New name")
	if err != nil || g.Version != 2 {
		t.Fatal("rename failed", err)
	}
	items, err := s.Groups(t.Context(), owner, "", 1)
	if err != nil || len(items) != 1 || items[0] != g {
		t.Fatal("group keyset page failed", items, err)
	}
	items, err = s.Groups(t.Context(), owner, g.ID, 1)
	if err != nil || len(items) != 0 {
		t.Fatal("group page repeated ID", err)
	}
	requireError(t, s.DeleteGroup(t.Context(), owner, g.ID, 1), auth.ErrConflict)
	if err := s.DeleteGroup(t.Context(), owner, g.ID, 2); err != nil {
		t.Fatal(err)
	}
	_, err = s.Group(t.Context(), owner, g.ID)
	requireError(t, err, sql.ErrNoRows)
	key = store.NewID()
	lib, err := s.CreateLibrary(t.Context(), owner, "Library", true, key)
	if err != nil {
		t.Fatal(err)
	}
	libAgain, err := s.CreateLibrary(t.Context(), owner, "Library", true, key)
	if err != nil || libAgain != lib {
		t.Fatal("library retry was not idempotent", err)
	}
	name := "Renamed library"
	updated, err := s.UpdateLibrary(t.Context(), owner, lib.ID, 1, LibraryPatch{Name: &name})
	if err != nil || updated.Version != 2 || updated.Name != name {
		t.Fatal("library rename failed", updated, err)
	}
	requireError(t, s.DeleteLibrary(t.Context(), owner, lib.ID, 1), auth.ErrConflict)
	if err := s.DeleteLibrary(t.Context(), owner, lib.ID, 2); err != nil {
		t.Fatal(err)
	}
	_, err = s.AdminLibrary(t.Context(), owner, lib.ID)
	requireError(t, err, sql.ErrNoRows)
}

func TestPolicyNoOpsValidationAndScopeEvents(t *testing.T) {
	s, owner := fixture(t)
	first, second, lib := group(t, s, owner, "First"), group(t, s, owner, "Second"), library(t, s, owner, "Library")
	viewer := reader(t, s, "reader")
	current := membership(t, s, owner, viewer, second.ID, first.ID)
	again, err := s.SetUserGroups(t.Context(), owner, viewer.User.ID, current.Version, []string{first.ID, second.ID})
	if err != nil || again.ScopeID != current.ScopeID || again.Version != current.Version {
		t.Fatal("equivalent membership changed epoch", again, err)
	}
	_, err = s.SetUserGroups(t.Context(), owner, viewer.User.ID, 1, []string{})
	requireError(t, err, auth.ErrConflict)
	for _, ids := range [][]string{nil, {first.ID, first.ID}, {""}, {strings.Repeat("a", 65)}, {"unknown"}} {
		_, err := s.SetUserGroups(t.Context(), owner, viewer.User.ID, current.Version, ids)
		if errors.Is(err, ErrGroupSet) || errors.Is(err, ErrUnknownGroup) {
			continue
		}
		t.Fatalf("invalid group set accepted: %v %v", ids, err)
	}
	id := album(t, s, lib.ID, "Album")
	_, err = s.SetAlbumPolicy(t.Context(), owner, id, 1, false, []string{first.ID})
	requireError(t, err, ErrRestriction)
	_, err = s.SetAlbumPolicy(t.Context(), owner, id, 1, true, []string{"unknown"})
	requireError(t, err, ErrUnknownGroup)
	policy, err := s.SetLibraryPolicy(t.Context(), owner, lib.ID, 1, []string{first.ID})
	if err != nil {
		t.Fatal(err)
	}
	noOp, err := s.SetLibraryPolicy(t.Context(), owner, lib.ID, policy.Version, []string{first.ID})
	if err != nil || noOp.Version != policy.Version {
		t.Fatal("library no-op advanced version", err)
	}
	latest, err := s.UserGroups(t.Context(), owner, viewer.User.ID)
	if err != nil || latest.ScopeID == current.ScopeID || latest.Version != current.Version+1 {
		t.Fatal("new library grantee's scope was not bumped", latest, err)
	}
	var oldScope string
	if err := s.db.QueryRow(`SELECT actor_scope FROM change_log WHERE kind='scope' AND resource_id=? ORDER BY seq DESC LIMIT 1`, viewer.User.ID).Scan(&oldScope); err != nil || oldScope != current.ScopeID {
		t.Fatal("old-scope revoke is missing", oldScope, err)
	}
}

func TestBulkScopeRevocationIsAtomicAndPublishesEveryOutboxRow(t *testing.T) {
	s, owner := fixture(t)
	g, lib := group(t, s, owner, "Bulk members"), library(t, s, owner, "Bulk library")
	if _, err := s.db.Exec(`WITH RECURSIVE n(value) AS (SELECT 1 UNION ALL SELECT value+1 FROM n WHERE value<2000)
	 INSERT INTO users(id,username,password_hash,role,created_at,updated_at,scope_id)
	 SELECT 'member-'||value,'member-'||value,u.password_hash,'user',0,0,'old-scope-'||value FROM n CROSS JOIN users u WHERE u.username='owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO user_groups(user_id,group_id) SELECT id,? FROM users WHERE role='user'; CREATE TRIGGER fail_bulk_outbox BEFORE INSERT ON outbox_events WHEN NEW.id LIKE 'scope:%' BEGIN SELECT RAISE(ABORT,'injected scope outbox failure'); END`, g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLibraryPolicy(t.Context(), owner, lib.ID, 1, []string{g.ID}); err == nil {
		t.Fatal("bulk grant committed without revoke outbox")
	}
	var old, version, orphans int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM users WHERE scope_id LIKE 'old-scope-%'),(SELECT version FROM libraries WHERE id=?),(SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE o.id IS NULL)`, lib.ID).Scan(&old, &version, &orphans); err != nil || old != 2000 || version != 1 || orphans != 0 {
		t.Fatalf("partial bulk mutation: old=%d version=%d orphans=%d %v", old, version, orphans, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_bulk_outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLibraryPolicy(t.Context(), owner, lib.ID, 1, []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM users WHERE role='user' AND version=2 AND length(scope_id)=64),(SELECT COUNT(*) FROM change_log WHERE kind='scope'),(SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE o.id IS NULL)`).Scan(&old, &version, &orphans); err != nil || old != 2000 || version != 2000 || orphans != 0 {
		t.Fatalf("missing bulk effects: users=%d revokes=%d orphans=%d %v", old, version, orphans, err)
	}
}

func TestPolicyDeletionAndMutationsRollBackOnEventAuditOrReceiptFailure(t *testing.T) {
	s, owner := fixture(t)
	g, alternate, lib := group(t, s, owner, "Family"), group(t, s, owner, "Alternate"), library(t, s, owner, "Library")
	viewer := reader(t, s, "reader")
	before := membership(t, s, owner, viewer, g.ID)
	lp, err := s.SetLibraryPolicy(t.Context(), owner, lib.ID, 1, []string{g.ID})
	if err != nil {
		t.Fatal(err)
	}
	id := album(t, s, lib.ID, "Private")
	ap, err := s.SetAlbumPolicy(t.Context(), owner, id, 1, true, []string{g.ID})
	if err != nil {
		t.Fatal(err)
	}
	before, err = s.UserGroups(t.Context(), owner, viewer.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"change_log", "outbox_events", "audit_events", "mutation_receipts"} {
		if _, err := s.db.Exec(`CREATE TRIGGER fail_policy BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected policy failure'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateGroup(t.Context(), owner, "Not committed", store.NewID()); err == nil {
			t.Fatal("group creation committed without", table)
		}
		if _, err := s.CreateLibrary(t.Context(), owner, "Not committed", true, store.NewID()); err == nil {
			t.Fatal("library creation committed without", table)
		}
		if table != "mutation_receipts" {
			if err := s.DeleteGroup(t.Context(), owner, g.ID, g.Version); err == nil {
				t.Fatal("group deletion committed without", table)
			}
			if _, err := s.SetUserGroups(t.Context(), owner, viewer.User.ID, before.Version, []string{}); err == nil {
				t.Fatal("membership committed without", table)
			}
			if _, err := s.SetLibraryPolicy(t.Context(), owner, lib.ID, lp.Version, []string{alternate.ID}); err == nil {
				t.Fatal("library policy committed without", table)
			}
			if _, err := s.SetAlbumPolicy(t.Context(), owner, id, ap.Version, true, []string{}); err == nil {
				t.Fatal("album policy committed without", table)
			}
		}
		membership, err := s.UserGroups(t.Context(), owner, viewer.User.ID)
		if err != nil || membership.Version != before.Version || membership.ScopeID != before.ScopeID || !slices.Equal(membership.GroupIDs, before.GroupIDs) {
			t.Fatal("partial member epoch", membership, err)
		}
		libraryPolicy, err := s.LibraryPolicy(t.Context(), owner, lib.ID)
		if err != nil || libraryPolicy.Version != lp.Version || !slices.Equal(libraryPolicy.GroupIDs, lp.GroupIDs) {
			t.Fatal("partial library policy", libraryPolicy, err)
		}
		albumPolicy, err := s.AlbumPolicy(t.Context(), owner, id)
		if err != nil || albumPolicy.Version != ap.Version || albumPolicy.Restricted != ap.Restricted || !slices.Equal(albumPolicy.GroupIDs, ap.GroupIDs) {
			t.Fatal("partial album policy", albumPolicy, err)
		}
		if _, err := s.db.Exec(`DROP TRIGGER fail_policy`); err != nil {
			t.Fatal(err)
		}
	}
	requireAlbums(t, s, viewer, id)
}

func TestAccessAdministrationRejectsForgedStaleAndDisabledActors(t *testing.T) {
	s, owner := fixture(t)
	g, lib := group(t, s, owner, "Family"), library(t, s, owner, "Library")
	id := album(t, s, lib.ID, "Album")
	viewer := reader(t, s, "reader")
	viewer.User = owner.User
	checks := []func(auth.Session) error{
		func(actor auth.Session) error { _, e := s.Groups(t.Context(), actor, "", 30); return e },
		func(actor auth.Session) error { _, e := s.Group(t.Context(), actor, g.ID); return e },
		func(actor auth.Session) error {
			_, e := s.CreateGroup(t.Context(), actor, "Forbidden", store.NewID())
			return e
		},
		func(actor auth.Session) error {
			_, e := s.RenameGroup(t.Context(), actor, g.ID, 1, "Forbidden")
			return e
		},
		func(actor auth.Session) error { return s.DeleteGroup(t.Context(), actor, g.ID, 1) },
		func(actor auth.Session) error { _, e := s.UserGroups(t.Context(), actor, owner.User.ID); return e },
		func(actor auth.Session) error {
			_, e := s.SetUserGroups(t.Context(), actor, owner.User.ID, 1, []string{})
			return e
		},
		func(actor auth.Session) error { _, e := s.LibraryPolicy(t.Context(), actor, lib.ID); return e },
		func(actor auth.Session) error {
			_, e := s.SetLibraryPolicy(t.Context(), actor, lib.ID, 1, []string{})
			return e
		},
		func(actor auth.Session) error { _, e := s.AlbumPolicy(t.Context(), actor, id); return e },
		func(actor auth.Session) error {
			_, e := s.SetAlbumPolicy(t.Context(), actor, id, 1, false, []string{})
			return e
		},
		func(actor auth.Session) error { _, e := s.AdminLibraries(t.Context(), actor, "", 30); return e },
		func(actor auth.Session) error { _, e := s.AdminLibrary(t.Context(), actor, lib.ID); return e },
		func(actor auth.Session) error {
			_, e := s.CreateLibrary(t.Context(), actor, "Forbidden", true, store.NewID())
			return e
		},
		func(actor auth.Session) error {
			name := "Forbidden"
			_, e := s.UpdateLibrary(t.Context(), actor, lib.ID, 1, LibraryPatch{Name: &name})
			return e
		},
		func(actor auth.Session) error { return s.DeleteLibrary(t.Context(), actor, lib.ID, 1) },
	}
	for i, check := range checks {
		if err := check(viewer); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("forged admin check %d: %v", i, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE users SET role='user' WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for i, check := range checks {
		if err := check(owner); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("stale admin check %d: %v", i, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE users SET active=0 WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for i, check := range checks {
		if err := check(owner); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("disabled admin check %d: %v", i, err)
		}
	}
}

func TestConcurrentGroupDeletionCannotOpenRestrictedAlbums(t *testing.T) {
	s, owner := fixture(t)
	base, restricted, lib := group(t, s, owner, "Base"), group(t, s, owner, "Restricted"), library(t, s, owner, "Library")
	if _, err := s.SetLibraryPolicy(t.Context(), owner, lib.ID, 1, []string{base.ID}); err != nil {
		t.Fatal(err)
	}
	viewer := reader(t, s, "reader")
	membership(t, s, owner, viewer, base.ID, restricted.ID)
	id := album(t, s, lib.ID, "Private")
	policy, err := s.SetAlbumPolicy(t.Context(), owner, id, 1, true, []string{restricted.ID})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Go(func() {
		<-start
		if err := s.DeleteGroup(t.Context(), owner, restricted.ID, 1); err != nil {
			t.Error(err)
		}
	})
	wg.Go(func() {
		<-start
		_, err := s.SetAlbumPolicy(t.Context(), owner, id, policy.Version, true, []string{restricted.ID})
		if err != nil && !errors.Is(err, auth.ErrConflict) && !errors.Is(err, ErrUnknownGroup) {
			t.Error(err)
		}
	})
	close(start)
	wg.Wait()
	policy, err = s.AlbumPolicy(t.Context(), owner, id)
	if err != nil || !policy.Restricted || len(policy.GroupIDs) != 0 {
		t.Fatal("concurrent deletion opened album", policy, err)
	}
	requireAlbums(t, s, viewer)
}

func TestNamesPaginationAndAnonymousReadsAreBounded(t *testing.T) {
	s, owner := fixture(t)
	for _, name := range []string{"", "  ", "a\x00b", "a\nb", strings.Repeat("界", 101), string([]byte{0xff})} {
		_, err := s.CreateGroup(t.Context(), owner, name, store.NewID())
		requireError(t, err, ErrName)
	}
	for _, limit := range []int{0, 102} {
		_, err := s.Groups(t.Context(), owner, "", limit)
		requireError(t, err, ErrGroupSet)
		_, err = s.Libraries(t.Context(), owner, "", limit)
		requireError(t, err, ErrGroupSet)
		_, err = s.Albums(t.Context(), owner, AlbumFilter{}, AlbumPosition{}, limit)
		requireError(t, err, ErrGroupSet)
	}
	_, err := s.Libraries(t.Context(), auth.Session{}, "", 30)
	requireError(t, err, auth.ErrUnauthenticated)
	_, err = s.Albums(t.Context(), auth.Session{}, AlbumFilter{}, AlbumPosition{}, 30)
	requireError(t, err, auth.ErrUnauthenticated)
	_, err = s.CreateGroup(t.Context(), owner, "Invalid receipt", "short")
	requireError(t, err, store.ErrIdempotencyKey)
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = fmt.Sprint(i)
	}
	_, _, err = normalizedGroups(ids)
	requireError(t, err, ErrGroupSet)
	owner.ExpiresAt = time.Now().Add(time.Hour)
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at=0`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Albums(t.Context(), owner, AlbumFilter{}, AlbumPosition{}, 30)
	requireError(t, err, auth.ErrUnauthenticated)
}
