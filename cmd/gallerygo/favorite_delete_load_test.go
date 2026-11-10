//go:build favorite_delete_load

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

// This test runs a real compiled server and isolated SQLite behind HTTPS. The
// unchanged 10s server deadline, not a test-only shortcut, bounds the mutation.
func TestCompiledFavoriteDelete100K(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "gallerygo")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, output)
	}
	assets := filepath.Join(dir, "web")
	if err := os.Mkdir(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<!doctype html><title>Isolated favorite load</title>"), 0o400); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	upstream, err := url.Parse("http://" + address)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(upstream))
	defer proxy.Close()
	client := proxy.Client()
	client.Timeout = scanHTTPSRequestTimeout
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "data", "gallerygo.db")
	settings := map[string]any{
		"listen": address, "base_url": proxy.URL, "database": path,
		"cache_root": filepath.Join(dir, "cache"), "web_root": assets,
		"worker":    map[string]any{"embedded": false, "required": false},
		"scheduler": map[string]any{"enabled": false},
	}
	config := filepath.Join(dir, "config.yaml")
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	password := rand.Text()
	for _, args := range [][]string{{"migrate", "--config", config}, {"bootstrap-admin", "--config", config, "--username", "owner", "--password-stdin"}} {
		command := exec.CommandContext(t.Context(), binary, args...)
		if args[0] == "bootstrap-admin" {
			command.Stdin = bytes.NewBufferString(password + "\n")
		}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", args[0], err, output)
		}
	}
	stop, kill := startFavoriteLoadServer(t, binary, config)
	defer stop()
	waitFavoriteLoadServer(t, upstream.String())
	api := &scanHTTPSClient{t: t, client: client, origin: proxy.URL}
	api.request("POST", "/auth/login", map[string]string{"username": "owner", "password": password}, 200, nil)
	var csrf struct {
		Token string `json:"token"`
	}
	api.request("GET", "/auth/csrf", nil, 200, &csrf)
	api.csrf = csrf.Token
	var owner auth.User
	api.request("GET", "/auth/me", nil, 200, &owner)
	var root access.FavoriteCollection
	headers := api.request("POST", "/favorite-collections", map[string]string{"name": "Compiled load"}, 201, &root)
	etag := headers.Get("ETag")
	if root.ID == "" || etag == "" {
		t.Fatal("compiled collection omitted its identity or validator")
	}
	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('compiled-favorite-load','Load',0);
 WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<99999)
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at)
 SELECT printf('compiled-favorite-%06d',i),'compiled-favorite-load','Album',0,0 FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO favorites(user_id,album_id,collection_id,created_at,updated_at,version)
 SELECT ?,id,?,0,0,1 FROM albums WHERE id LIKE 'compiled-favorite-%'`, owner.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	var preview access.FavoriteCollectionDeletionPreview
	api.request("GET", "/favorite-collections/"+root.ID+"/deletion-preview?action=remove_favorites", nil, 200, &preview)
	if preview.Action != access.FavoriteDeleteRemove || len(preview.ConfirmationToken) != 64 {
		t.Fatal("compiled preview did not return a confirmation", preview)
	}
	check := func(deleted bool) {
		t.Helper()
		var folders, old, cleared, events, uniqueEvents, outbox, audits, receipts int
		if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM favorite_collections WHERE user_id=? AND id=?),
 (SELECT COUNT(*) FROM favorites WHERE user_id=? AND collection_id=? AND favorited=1 AND version=1),
 (SELECT COUNT(*) FROM favorites WHERE user_id=? AND collection_id IS NULL AND favorited=0 AND version=2),
 (SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND kind='favorite' AND operation='delete' AND resource_id LIKE 'compiled-favorite-%'),
 (SELECT COUNT(DISTINCT resource_id) FROM change_log WHERE actor_scope=? AND kind='favorite' AND operation='delete' AND resource_id LIKE 'compiled-favorite-%'),
 (SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.actor_scope=? AND c.kind='favorite' AND c.operation='delete' AND c.resource_id LIKE 'compiled-favorite-%'),
 (SELECT COUNT(*) FROM audit_events WHERE actor_user_id=? AND event_type='favorite_collection_deleted'),
 (SELECT COUNT(*) FROM mutation_receipts WHERE actor_user_id=? AND operation='favorite_collection.delete')`, owner.ID, root.ID, owner.ID, root.ID, owner.ID, owner.ScopeID, owner.ScopeID, owner.ScopeID, owner.ID, owner.ID).Scan(&folders, &old, &cleared, &events, &uniqueEvents, &outbox, &audits, &receipts); err != nil {
			t.Fatal(err)
		}
		got := [8]int{folders, old, cleared, events, uniqueEvents, outbox, audits, receipts}
		want := [8]int{1, 100000, 0, 0, 0, 0, 0, 0}
		if deleted {
			want = [8]int{0, 0, 100000, 100000, 100000, 100000, 1, 1}
		}
		if got != want {
			t.Fatal("compiled deletion lost atomic data or private events", got, want)
		}
	}
	key := store.NewID()
	extra := http.Header{"If-Match": {etag}, "Idempotency-Key": {key}}
	body := map[string]string{"action": access.FavoriteDeleteRemove, "confirmation_token": preview.ConfirmationToken}
	killFavoriteDeleteAfterWALSpill(t, db, path+"-wal", client, proxy.URL, api.csrf, root.ID, etag, key, preview.ConfirmationToken, kill)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	check(false)
	stop, _ = startFavoriteLoadServer(t, binary, config)
	defer stop()
	waitFavoriteLoadServer(t, upstream.String())
	var afterRestart access.FavoriteCollectionDeletionPreview
	api.request("GET", "/favorite-collections/"+root.ID+"/deletion-preview?action=remove_favorites", nil, 200, &afterRestart)
	if afterRestart != preview {
		t.Fatal("restart changed the server-owned confirmation or original snapshot")
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_compiled_favorite_receipt BEFORE INSERT ON mutation_receipts
 WHEN NEW.operation='favorite_collection.delete' BEGIN SELECT RAISE(ABORT,'/private/SQL/compiled-favorite-receipt'); END`); err != nil {
		t.Fatal(err)
	}
	var failure map[string]any
	started := time.Now()
	responseHeaders := api.request("DELETE", "/favorite-collections/"+root.ID, body, 500, &failure, extra)
	rollbackElapsed := time.Since(started)
	if failure["request_id"] == "" || failure["request_id"] != responseHeaders.Get("X-Request-ID") {
		t.Fatal("late failure lost its request correlation")
	}
	delete(failure, "request_id")
	diagnostics, err := json.Marshal(failure)
	if err != nil || bytes.Contains(diagnostics, []byte("/private/")) || bytes.Contains(diagnostics, []byte("SQL")) || bytes.Contains(diagnostics, []byte(preview.ConfirmationToken)) {
		t.Fatal("compiled late failure exposed internal diagnostics")
	}
	check(false)
	if _, err := db.Exec(`DROP TRIGGER fail_compiled_favorite_receipt`); err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	api.request("DELETE", "/favorite-collections/"+root.ID, body, 204, nil, extra)
	commitElapsed := time.Since(started)
	if commitElapsed >= 10*time.Second {
		t.Fatal("server reported successful deletion after its metadata deadline", commitElapsed)
	}
	api.request("DELETE", "/favorite-collections/"+root.ID, body, 204, nil, extra)
	check(true)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	stop()
	db, err = store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	check(true)
	t.Log("compiled 100k favorite deletion: SIGKILL/reopen rollback; late failure", rollbackElapsed, "successful retry", commitElapsed)
}
