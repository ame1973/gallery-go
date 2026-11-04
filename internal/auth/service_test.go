package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/store"
	"gallerygo/migrations"
	"golang.org/x/crypto/bcrypt"
)

const strongPassword = "a careful private gallery passphrase"

func testService(t *testing.T) (*Service, User) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	user, err := s.Bootstrap(t.Context(), "Owner", strongPassword)
	if err != nil {
		t.Fatal(err)
	}
	return s, user
}

func login(t *testing.T, s *Service) Session {
	t.Helper()
	session, err := s.Login(t.Context(), "owner", strongPassword, "127.0.0.1", "test browser", "")
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func assertError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPasswordBoundaries(t *testing.T) {
	for _, password := range []string{"", "admin123", strings.Repeat("x", 14), strings.Repeat("x", 73), strings.Repeat("界", 25), string([]byte{0xff})} {
		_, err := HashPassword(password)
		assertError(t, err, ErrWeakPassword)
	}
	for _, password := range []string{strings.Repeat("x", 15), strings.Repeat("界", 24), "  a passphrase with spaces  "} {
		hash, err := HashPassword(password)
		if err != nil {
			t.Fatal(err)
		}
		cost, err := bcrypt.Cost([]byte(hash))
		if err != nil || cost != 12 {
			t.Fatalf("cost=%d %v", cost, err)
		}
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
			t.Fatal(err)
		}
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("!"+password[1:])); err == nil {
			t.Fatal("wrong password matched")
		}
	}
}

func TestBootstrapOnceWithAtomicEvents(t *testing.T) {
	s, user := testService(t)
	if user.Username != "owner" || user.Role != "admin" || !user.Active || !user.DownloadOriginal {
		t.Fatalf("unexpected user %+v", user)
	}
	_, err := s.Bootstrap(t.Context(), "second", strongPassword)
	assertError(t, err, ErrBootstrapped)
	var changes, outbox, audit int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`).Scan(&changes, &outbox, &audit); err != nil {
		t.Fatal(err)
	}
	if changes != 1 || outbox != 1 || audit != 1 {
		t.Fatalf("changes=%d outbox=%d audit=%d", changes, outbox, audit)
	}
	if _, err := s.db.Exec(`DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Bootstrap(t.Context(), "recreated", strongPassword)
	assertError(t, err, ErrBootstrapped)
}

func TestConcurrentBootstrap(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, username := range []string{"first", "second"} {
		wg.Go(func() { _, err := s.Bootstrap(t.Context(), username, strongPassword); results <- err })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else {
			assertError(t, err, ErrBootstrapped)
		}
	}
	if successes != 1 {
		t.Fatalf("successful bootstraps=%d", successes)
	}
}

func TestSessionSecrecyRotationAndCSRF(t *testing.T) {
	s, _ := testService(t)
	first := login(t, s)
	if !validToken(first.Token) || !validToken(first.CSRFToken()) {
		t.Fatal("tokens are not 256-bit values")
	}
	var stored, csrf string
	if err := s.db.QueryRow(`SELECT id_hash,csrf_hash FROM sessions WHERE id=?`, first.ID).Scan(&stored, &csrf); err != nil {
		t.Fatal(err)
	}
	if stored == first.Token || stored != digest(first.Token) || csrf == first.CSRFToken() {
		t.Fatal("plaintext token persisted")
	}
	payload, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{first.Token, first.CSRFToken(), stored, csrf, strongPassword} {
		if strings.Contains(string(payload), secret) {
			t.Fatal("session DTO leaked a secret")
		}
	}
	authenticated, err := s.Authenticate(t.Context(), first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !authenticated.ValidCSRF(first.CSRFToken()) || authenticated.ValidCSRF(token()) || authenticated.ValidCSRF("") {
		t.Fatal("CSRF binding failed")
	}
	second, err := s.Login(t.Context(), "OWNER", strongPassword, "127.0.0.1", "second browser", first.Token)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token || first.ID == second.ID {
		t.Fatal("session not rotated")
	}
	_, err = s.Authenticate(t.Context(), first.Token)
	assertError(t, err, ErrUnauthenticated)
	if second.ValidCSRF(first.CSRFToken()) {
		t.Fatal("CSRF crossed sessions")
	}
	for _, invalid := range []string{"1", "", strings.Repeat("a", 43), token()} {
		_, err := s.Authenticate(t.Context(), invalid)
		assertError(t, err, ErrUnauthenticated)
	}
}

func TestSessionExpiryAndDisabledAccount(t *testing.T) {
	s, user := testService(t)
	clock := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	idle := login(t, s)
	clock = clock.Add(s.policy.Idle)
	_, err := s.Authenticate(t.Context(), idle.Token)
	assertError(t, err, ErrUnauthenticated)
	absolute := login(t, s)
	clock = clock.Add(s.policy.Absolute)
	if _, err := s.db.Exec(`UPDATE sessions SET last_seen_at=? WHERE id=?`, clock.UnixMilli(), absolute.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(t.Context(), absolute.Token)
	assertError(t, err, ErrUnauthenticated)
	active := login(t, s)
	if _, err := s.db.Exec(`UPDATE users SET active=0 WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(t.Context(), active.Token)
	assertError(t, err, ErrUnauthenticated)
	_, err = s.Login(t.Context(), "owner", strongPassword, "127.0.0.1", "test", "")
	assertError(t, err, ErrCredentials)
}

func TestRollingIdleAndFreshRole(t *testing.T) {
	s, user := testService(t)
	clock := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	session := login(t, s)
	clock = clock.Add(23 * time.Hour)
	if _, err := s.Authenticate(t.Context(), session.Token); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(23 * time.Hour)
	if _, err := s.db.Exec(`UPDATE users SET role='user',version=version+1 WHERE id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.Authenticate(t.Context(), session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if current.User.Role != "user" || current.User.Version != 2 {
		t.Fatal("stale account authorization")
	}
}

func TestPasswordChangeRevokesOthersAndRotatesCurrent(t *testing.T) {
	s, _ := testService(t)
	first := login(t, s)
	other := login(t, s)
	_, err := s.ChangePassword(t.Context(), first, "incorrect old password", strongPassword+"new")
	assertError(t, err, ErrCredentials)
	changed, err := s.ChangePassword(t.Context(), first, strongPassword, "the replacement private gallery phrase")
	if err != nil {
		t.Fatal(err)
	}
	if changed.ID != first.ID || changed.Token == first.Token || changed.User.ScopeID == first.User.ScopeID || changed.User.Version != 2 || !changed.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatal("invalid password-change rotation")
	}
	for _, old := range []string{first.Token, other.Token} {
		_, err := s.Authenticate(t.Context(), old)
		assertError(t, err, ErrUnauthenticated)
	}
	if _, err := s.Authenticate(t.Context(), changed.Token); err != nil {
		t.Fatal(err)
	}
	if changed.ValidCSRF(first.CSRFToken()) {
		t.Fatal("old CSRF still valid")
	}
	var revokedScope string
	if err := s.db.QueryRow(`SELECT actor_scope FROM change_log WHERE kind='scope' AND operation='revoke' AND resource_id=?`, first.User.ID).Scan(&revokedScope); err != nil || revokedScope != first.User.ScopeID {
		t.Fatalf("missing old-scope revoke: %q %v", revokedScope, err)
	}
	_, err = s.Login(t.Context(), "owner", strongPassword, "127.0.0.1", "test", "")
	assertError(t, err, ErrCredentials)
	if _, err := s.Login(t.Context(), "owner", "the replacement private gallery phrase", "127.0.0.1", "test", ""); err != nil {
		t.Fatal(err)
	}
}

func TestSessionListAndCrossAccountRevoke(t *testing.T) {
	s, _ := testService(t)
	first := login(t, s)
	second := login(t, s)
	items, err := s.Sessions(t.Context(), first, "", 1)
	if err != nil || len(items) != 1 || items[0].ID != first.ID || !items[0].Current {
		t.Fatalf("items=%+v error=%v", items, err)
	}
	items, err = s.Sessions(t.Context(), first, items[0].ID, 10)
	if err != nil || len(items) != 1 || items[0].ID != second.ID || items[0].Current {
		t.Fatalf("items=%+v error=%v", items, err)
	}
	otherID := store.NewID()
	if _, err := s.db.Exec(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at,scope_id) SELECT ?, 'other',password_hash,'user',0,0,? FROM users LIMIT 1`, otherID, token()); err != nil {
		t.Fatal(err)
	}
	other, err := s.Login(t.Context(), "other", strongPassword, "127.0.0.2", "other", "")
	if err != nil {
		t.Fatal(err)
	}
	assertError(t, s.Revoke(t.Context(), first, other.ID), sql.ErrNoRows)
	if err := s.Revoke(t.Context(), first, second.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(t.Context(), second.Token)
	assertError(t, err, ErrUnauthenticated)
	if err := s.Revoke(t.Context(), first, first.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.Authenticate(t.Context(), first.Token)
	assertError(t, err, ErrUnauthenticated)
}

func TestRateLimitsArePersistentAndExpire(t *testing.T) {
	s, _ := testService(t)
	clock := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	for range 10 {
		if err := s.reserveLogin(t.Context(), "owner", "127.0.0.1"); err != nil {
			t.Fatal(err)
		}
	}
	assertError(t, s.reserveLogin(t.Context(), "owner", "127.0.0.2"), ErrRateLimited)
	fresh, err := New(s.db, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	fresh.now = s.now
	assertError(t, fresh.reserveLogin(t.Context(), "owner", "127.0.0.3"), ErrRateLimited)
	clock = clock.Add(rateWindow)
	if err := s.reserveLogin(t.Context(), "owner", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	for i := range 49 {
		if err := s.reserveLogin(t.Context(), store.NewID(), "127.0.0.1"); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	assertError(t, s.reserveLogin(t.Context(), "another-user", "127.0.0.1"), ErrRateLimited)
}

func TestFailedLoginAuditAndAtomicRollback(t *testing.T) {
	s, _ := testService(t)
	for _, username := range []string{"owner", "absent"} {
		_, err := s.Login(t.Context(), username, "not the password", "127.0.0.1", "test", "")
		assertError(t, err, ErrCredentials)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE event_type='login_failed'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("failed audit count=%d %v", count, err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox_events BEGIN SELECT RAISE(ABORT,'injected outbox failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(t.Context(), "owner", strongPassword, "127.0.0.1", "test", ""); err == nil {
		t.Fatal("login committed without outbox")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial session persisted: %d %v", count, err)
	}
}

func TestUsernamePolicyAndCancelledPasswordWork(t *testing.T) {
	for _, username := range []string{"ab", "../owner", "test user", strings.Repeat("a", 65), "管理員"} {
		_, err := NormalizeUsername(username)
		assertError(t, err, ErrUsername)
	}
	name, err := NormalizeUsername(" OWNER-1._ ")
	if err != nil || name != "owner-1._" {
		t.Fatalf("username %q %v", name, err)
	}
	for _, p := range []Policy{{}, {Idle: 25 * time.Hour, Absolute: 30 * 24 * time.Hour}, {Idle: time.Hour, Absolute: time.Minute}, {Idle: time.Hour, Absolute: 31 * 24 * time.Hour}} {
		if p.Validate() == nil {
			t.Fatal("invalid session policy accepted")
		}
	}
	s := &Service{passwordSlots: make(chan struct{}, 1)}
	s.passwordSlots <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.passwordSlot(ctx)
	assertError(t, err, context.Canceled)
}
