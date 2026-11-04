package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func managedUser(t *testing.T, s *Service, actor Session, name, role string) User {
	t.Helper()
	user, err := s.CreateUser(t.Context(), actor, NewUser{Username: name, Password: strongPassword, Role: role, Active: true}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func managedLogin(t *testing.T, s *Service, user User) Session {
	t.Helper()
	session, err := s.Login(t.Context(), user.Username, strongPassword, "192.0.2.1", "managed browser", "")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func pointer[T any](value T) *T { return &value }

func TestAdminUsersLifecycleAndVersionedRevocation(t *testing.T) {
	s, _ := testService(t)
	admin := login(t, s)
	user := managedUser(t, s, admin, " Reader ", "user")
	if user.Username != "reader" || user.Role != "user" || !user.Active || user.DownloadOriginal || user.Version != 1 || user.ScopeID == admin.User.ScopeID {
		t.Fatalf("unsafe defaults: %+v", user)
	}
	reader := managedLogin(t, s, user)
	unchanged, err := s.UpdateUser(t.Context(), admin, user.ID, 1, UserPatch{Active: pointer(true)})
	if err != nil || unchanged != user {
		t.Fatalf("no-op changed account: %+v %v", unchanged, err)
	}
	if _, err := s.Authenticate(t.Context(), reader.Token); err != nil {
		t.Fatal("no-op revoked session", err)
	}
	disabled, err := s.UpdateUser(t.Context(), admin, user.ID, 1, UserPatch{Active: pointer(false), DownloadOriginal: pointer(true)})
	if err != nil || disabled.Active || !disabled.DownloadOriginal || disabled.Version != 2 || disabled.ScopeID == user.ScopeID {
		t.Fatalf("disable: %+v %v", disabled, err)
	}
	_, err = s.Authenticate(t.Context(), reader.Token)
	assertError(t, err, ErrUnauthenticated)
	_, err = s.Login(t.Context(), user.Username, strongPassword, "192.0.2.1", "test", "")
	assertError(t, err, ErrCredentials)
	_, err = s.UpdateUser(t.Context(), admin, user.ID, 1, UserPatch{Active: pointer(true)})
	assertError(t, err, ErrConflict)
	updated, err := s.UpdateUser(t.Context(), admin, user.ID, 2, UserPatch{Active: pointer(true), Username: pointer("Renamed"), Role: pointer("admin"), DownloadOriginal: pointer(false)})
	if err != nil || updated.Username != "renamed" || updated.Role != "admin" || !updated.Active || updated.DownloadOriginal || updated.Version != 3 {
		t.Fatalf("update: %+v %v", updated, err)
	}
	_, err = s.Authenticate(t.Context(), reader.Token)
	assertError(t, err, ErrUnauthenticated)
	fresh := managedLogin(t, s, updated)
	if fresh.User != updated {
		t.Fatal("login used stale policy")
	}
	var revokes int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE kind='scope' AND operation='revoke' AND resource_id=?`, user.ID).Scan(&revokes); err != nil || revokes != 2 {
		t.Fatalf("revoke events=%d %v", revokes, err)
	}
	page, err := s.Users(t.Context(), admin, "", 1)
	if err != nil || len(page) != 1 || page[0].ID != admin.User.ID {
		t.Fatalf("first user page: %+v %v", page, err)
	}
	page, err = s.Users(t.Context(), admin, page[0].ID, 1)
	if err != nil || len(page) != 1 || page[0] != updated {
		t.Fatalf("second user page: %+v %v", page, err)
	}
	if _, err := s.GetUser(t.Context(), admin, "missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unknown account: %v", err)
	}
	_, err = s.UpdateUser(t.Context(), admin, user.ID, 3, UserPatch{Username: pointer("OWNER")})
	assertError(t, err, ErrUsernameTaken)
}

func TestAdminPostIdempotencyAndPasswordReset(t *testing.T) {
	s, _ := testService(t)
	admin := login(t, s)
	key := store.NewID()
	input := NewUser{Username: "reader", Password: strongPassword, Role: "user", Active: true}
	user, err := s.CreateUser(t.Context(), admin, input, key)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.CreateUser(t.Context(), admin, input, key)
	if err != nil || replay != user {
		t.Fatalf("create replay: %+v %v", replay, err)
	}
	input.Password += "x"
	_, err = s.CreateUser(t.Context(), admin, input, key)
	assertError(t, err, ErrIdempotencyReuse)
	input.Password = strongPassword
	input.Role = "admin"
	_, err = s.CreateUser(t.Context(), admin, input, key)
	assertError(t, err, ErrIdempotencyReuse)
	_, err = s.CreateUser(t.Context(), admin, input, store.NewID())
	assertError(t, err, ErrUsernameTaken)
	first, second := managedLogin(t, s, user), managedLogin(t, s, user)
	newPassword := "a distinct replacement account password"
	resetKey := store.NewID()
	changed, err := s.ResetPassword(t.Context(), admin, user.ID, user.Version, newPassword, resetKey)
	if err != nil || changed.Version != 2 || changed.ScopeID == user.ScopeID {
		t.Fatalf("reset: %+v %v", changed, err)
	}
	for _, session := range []Session{first, second} {
		_, err := s.Authenticate(t.Context(), session.Token)
		assertError(t, err, ErrUnauthenticated)
	}
	_, err = s.Login(t.Context(), user.Username, strongPassword, "192.0.2.2", "old credential", "")
	assertError(t, err, ErrCredentials)
	fresh, err := s.Login(t.Context(), user.Username, newPassword, "192.0.2.2", "new credential", "")
	if err != nil {
		t.Fatal(err)
	}
	replay, err = s.ResetPassword(t.Context(), admin, user.ID, user.Version, newPassword, resetKey)
	if err != nil || replay != changed {
		t.Fatalf("reset replay: %+v %v", replay, err)
	}
	if _, err := s.Authenticate(t.Context(), fresh.Token); err != nil {
		t.Fatal("a replay repeated session revocation", err)
	}
	_, err = s.ResetPassword(t.Context(), admin, user.ID, changed.Version, newPassword, key)
	assertError(t, err, ErrIdempotencyReuse)
	var hash, metadata, result string
	if err := s.db.QueryRow(`SELECT password_hash,request_json,result_json FROM admin_user_requests WHERE request_key=?`, resetKey).Scan(&hash, &metadata, &result); err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(newPassword)) != nil || strings.Contains(metadata+result, newPassword) || strings.Contains(metadata+result, hash) {
		t.Fatal("idempotency result contains credential material")
	}
	_, err = s.ResetPassword(t.Context(), admin, admin.User.ID, admin.User.Version, newPassword, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(t.Context(), admin.Token)
	assertError(t, err, ErrUnauthenticated)
}

func TestAdminLastActiveInvariantIncludingConcurrentWriters(t *testing.T) {
	for _, operation := range []string{"disable", "demote", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s, _ := testService(t)
			admin := login(t, s)
			mutate := func(actor Session) error {
				switch operation {
				case "disable":
					_, err := s.UpdateUser(t.Context(), actor, actor.User.ID, 1, UserPatch{Active: pointer(false)})
					return err
				case "demote":
					_, err := s.UpdateUser(t.Context(), actor, actor.User.ID, 1, UserPatch{Role: pointer("user")})
					return err
				default:
					return s.DeleteUser(t.Context(), actor, actor.User.ID, 1)
				}
			}
			assertError(t, mutate(admin), ErrLastAdmin)
			inactive := managedUser(t, s, admin, "inactive-admin", "admin")
			if _, err := s.UpdateUser(t.Context(), admin, inactive.ID, 1, UserPatch{Active: pointer(false)}); err != nil {
				t.Fatal(err)
			}
			assertError(t, mutate(admin), ErrLastAdmin)
			second := managedLogin(t, s, managedUser(t, s, admin, "another-admin", "admin"))
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for _, actor := range []Session{admin, second} {
				wg.Go(func() { <-start; results <- mutate(actor) })
			}
			close(start)
			wg.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				} else {
					assertError(t, err, ErrLastAdmin)
				}
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE active=1 AND role='admin'`).Scan(&count); err != nil || count != 1 || successes != 1 {
				t.Fatalf("active admins=%d successful writes=%d %v", count, successes, err)
			}
		})
	}
}

func TestAdminAuthorizationDoesNotTrustSessionDTO(t *testing.T) {
	s, _ := testService(t)
	admin := login(t, s)
	reader := managedLogin(t, s, managedUser(t, s, admin, "reader", "user"))
	reader.User.Role = "admin"
	checks := []func(Session) error{
		func(actor Session) error { _, e := s.Users(t.Context(), actor, "", 30); return e },
		func(actor Session) error { _, e := s.GetUser(t.Context(), actor, admin.User.ID); return e },
		func(actor Session) error {
			_, e := s.CreateUser(t.Context(), actor, NewUser{Username: "attempt", Password: strongPassword, Role: "user", Active: true}, store.NewID())
			return e
		},
		func(actor Session) error {
			_, e := s.UpdateUser(t.Context(), actor, admin.User.ID, 1, UserPatch{Active: pointer(false)})
			return e
		},
		func(actor Session) error { return s.DeleteUser(t.Context(), actor, admin.User.ID, 1) },
		func(actor Session) error {
			_, e := s.ResetPassword(t.Context(), actor, admin.User.ID, 1, strongPassword, store.NewID())
			return e
		},
		func(actor Session) error { _, e := s.UserSessions(t.Context(), actor, admin.User.ID, "", 30); return e },
		func(actor Session) error { return s.RevokeUserSession(t.Context(), actor, admin.User.ID, admin.ID) },
		func(actor Session) error { _, e := s.Audit(t.Context(), actor, math.MaxInt64, 30); return e },
	}
	for i, check := range checks {
		if err := check(reader); !errors.Is(err, ErrForbidden) {
			t.Fatalf("user check %d: %v", i, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE users SET role='user' WHERE id=?`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	for i, check := range checks {
		if err := check(admin); !errors.Is(err, ErrForbidden) {
			t.Fatalf("stale role check %d: %v", i, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE users SET active=0 WHERE id=?`, admin.User.ID); err != nil {
		t.Fatal(err)
	}
	for i, check := range checks {
		if err := check(admin); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("disabled admin check %d: %v", i, err)
		}
	}
}

func TestAdminAtomicEventsAndFailureRollback(t *testing.T) {
	s, _ := testService(t)
	admin := login(t, s)
	user := managedUser(t, s, admin, "reader", "user")
	reader := managedLogin(t, s, user)
	counts := func() [4]int {
		t.Helper()
		var values [4]int
		if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM users),(SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM audit_events),(SELECT COUNT(*) FROM admin_user_requests)`).Scan(&values[0], &values[1], &values[2], &values[3]); err != nil {
			t.Fatal(err)
		}
		return values
	}
	before := counts()
	for _, table := range []string{"outbox_events", "audit_events", "admin_user_requests"} {
		if _, err := s.db.Exec(`CREATE TRIGGER injected_failure BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
			t.Fatal(err)
		}
		_, err := s.CreateUser(t.Context(), admin, NewUser{Username: "not-created", Password: strongPassword, Role: "user", Active: true}, store.NewID())
		if err == nil {
			t.Fatal("create committed without", table)
		}
		_, err = s.ResetPassword(t.Context(), admin, user.ID, 1, "a reset that must roll back", store.NewID())
		if err == nil {
			t.Fatal("reset committed without", table)
		}
		if table != "admin_user_requests" {
			_, err = s.UpdateUser(t.Context(), admin, user.ID, 1, UserPatch{Active: pointer(false)})
			if err == nil {
				t.Fatal("update committed without", table)
			}
			if err := s.DeleteUser(t.Context(), admin, user.ID, 1); err == nil {
				t.Fatal("delete committed without", table)
			}
			if err := s.RevokeUserSession(t.Context(), admin, user.ID, reader.ID); err == nil {
				t.Fatal("session revocation committed without", table)
			}
		}
		if got := counts(); got != before {
			t.Fatalf("partial transaction: before=%v after=%v", before, got)
		}
		got, err := s.GetUser(t.Context(), admin, user.ID)
		if err != nil || got != user {
			t.Fatalf("partial account mutation: %+v %v", got, err)
		}
		if _, err := s.Authenticate(t.Context(), reader.Token); err != nil {
			t.Fatal("partial revocation", err)
		}
		if _, err := s.db.Exec(`DROP TRIGGER injected_failure`); err != nil {
			t.Fatal(err)
		}
	}
	var missing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE o.id IS NULL`).Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("events without outbox=%d %v", missing, err)
	}
}

func TestAdminDeleteOwnedMetadataPreservesGalleryAndAudit(t *testing.T) {
	s, _ := testService(t)
	admin := login(t, s)
	user := managedUser(t, s, admin, "reader", "user")
	reader := managedLogin(t, s, user)
	queries := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO libraries(id,name,created_at) VALUES('library','test library',0)`, nil},
		{`INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES('album','library','test',0,0)`, nil},
		{`INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('media','album','image','fixture.jpg',0,0)`, nil},
		{`INSERT INTO playlists(id,user_id,name,created_at) VALUES('playlist',?,'private',0),('retained',?,'retained',0)`, []any{user.ID, admin.User.ID}},
		{`INSERT INTO playlist_items(playlist_id,media_id,position) VALUES('playlist','media',0)`, nil},
		{`INSERT INTO playback_progress(user_id,playlist_id,media_id,updated_at) VALUES(?,'playlist','media',0)`, []any{user.ID}},
		{`INSERT INTO sync_operations(id,user_id,result_json,applied_at) VALUES('operation',?,'{}',0)`, []any{user.ID}},
		{`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('parent',?,NULL,'parent',0),('child',?,'parent','child',0)`, []any{user.ID, user.ID}},
		{`INSERT INTO favorites(user_id,album_id,collection_id,created_at) VALUES(?,'album','child',0)`, []any{user.ID}},
		{`INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES(?,'album',5,0)`, []any{user.ID}},
	}
	for _, query := range queries {
		if _, err := s.db.Exec(query.query, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	assertError(t, s.DeleteUser(t.Context(), admin, user.ID, 99), ErrConflict)
	if err := s.DeleteUser(t.Context(), admin, user.ID, 1); err != nil {
		t.Fatal(err)
	}
	_, err := s.Authenticate(t.Context(), reader.Token)
	assertError(t, err, ErrUnauthenticated)
	for table, want := range map[string]int{"users": 1, "sessions": 1, "playlists": 1, "playlist_items": 0, "playback_progress": 0, "sync_operations": 0, "favorite_collections": 0, "favorites": 0, "ratings": 0, "media": 1, "albums": 1, "libraries": 1} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d want=%d %v", table, count, want, err)
		}
	}
	var audited bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM audit_events WHERE actor_user_id=? AND event_type='login_succeeded')`, user.ID).Scan(&audited); err != nil || !audited {
		t.Fatal("deleted account's audit was lost", err)
	}
}

func TestAdminSessionsAuditAndInputBounds(t *testing.T) {
	s, _ := testService(t)
	admin := login(t, s)
	user := managedUser(t, s, admin, "reader", "user")
	reader := managedLogin(t, s, user)
	items, err := s.UserSessions(t.Context(), admin, user.ID, "", 30)
	if err != nil || len(items) != 1 || items[0].Current || items[0].ID != reader.ID {
		t.Fatalf("admin session list: %+v %v", items, err)
	}
	assertError(t, s.RevokeUserSession(t.Context(), admin, user.ID, admin.ID), sql.ErrNoRows)
	if err := s.RevokeUserSession(t.Context(), admin, user.ID, reader.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(t.Context(), reader.Token)
	assertError(t, err, ErrUnauthenticated)
	items, err = s.UserSessions(t.Context(), admin, user.ID, "", 30)
	if err != nil || len(items) != 0 {
		t.Fatalf("revoked session listed: %+v %v", items, err)
	}
	events, err := s.Audit(t.Context(), admin, math.MaxInt64, 1)
	if err != nil || len(events) != 1 || events[0].EventType != "admin_session_revoked" {
		t.Fatalf("audit: %+v %v", events, err)
	}
	data, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{strongPassword, admin.Token, admin.CSRFToken(), reader.Token, digest(reader.Token)} {
		if strings.Contains(string(data), secret) {
			t.Fatal("audit leaked credentials")
		}
	}
	for _, limit := range []int{0, 102} {
		_, err := s.Users(t.Context(), admin, "", limit)
		assertError(t, err, ErrInvalidUser)
		_, err = s.UserSessions(t.Context(), admin, user.ID, "", limit)
		assertError(t, err, ErrInvalidUser)
		_, err = s.Audit(t.Context(), admin, math.MaxInt64, limit)
		assertError(t, err, ErrInvalidUser)
	}
	_, err = s.UpdateUser(t.Context(), admin, user.ID, 1, UserPatch{})
	assertError(t, err, ErrInvalidUser)
	_, err = s.UpdateUser(t.Context(), admin, user.ID, 1, UserPatch{Role: pointer("superuser")})
	assertError(t, err, ErrInvalidUser)
	for _, key := range []string{"", "short", strings.Repeat("a", 129), "illegal/key-value"} {
		_, err := s.ResetPassword(t.Context(), admin, user.ID, 1, strongPassword, key)
		assertError(t, err, ErrIdempotencyKey)
	}
	_, err = s.ResetPassword(t.Context(), admin, user.ID, 1, "weak", store.NewID())
	assertError(t, err, ErrWeakPassword)
}

func TestAdminConcurrentIdempotencyAndRetention(t *testing.T) {
	s, _ := testService(t)
	clock := time.Now().UTC()
	s.now = func() time.Time { return clock }
	admin := login(t, s)
	key := store.NewID()
	input := NewUser{Username: "reader", Password: strongPassword, Role: "user", Active: true}
	type result struct {
		user User
		err  error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { user, err := s.CreateUser(t.Context(), admin, input, key); results <- result{user, err} })
	}
	wg.Wait()
	close(results)
	var winner User
	for result := range results {
		if result.err == nil {
			if winner.ID != "" && winner != result.user {
				t.Fatal("one key created different accounts")
			}
			winner = result.user
		} else {
			assertError(t, result.err, ErrRequestInFlight)
		}
	}
	if winner.ID == "" {
		t.Fatal("neither concurrent create succeeded")
	}
	replay, err := s.CreateUser(t.Context(), admin, input, key)
	if err != nil || replay != winner {
		t.Fatalf("concurrent replay: %+v %v", replay, err)
	}
	clock = clock.Add(24*time.Hour + time.Millisecond)
	admin = login(t, s)
	input.Username = "after-expiry"
	created, err := s.CreateUser(t.Context(), admin, input, key)
	if err != nil || created.ID == winner.ID {
		t.Fatalf("expired result reused: %+v %v", created, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM admin_user_requests`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("expired request retention: %d %v", count, err)
	}
}
