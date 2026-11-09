//go:build album_views_load

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
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

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

// This production-binary load check is run explicitly, without race instrumentation
// in the child, because the server's real 10s metadata deadline must remain intact.
// Its database and input files are created exclusively in the test's temporary dir.
func TestCompiledAlbumViewsAccountDelete100K(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "gallerygo")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, output)
	}
	assets := filepath.Join(dir, "web")
	if err := os.Mkdir(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<!doctype html><title>Isolated account load</title>"), 0o400); err != nil {
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
	stop := startCLIProcess(t, binary, config, "serve")
	defer stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	probe := &http.Client{Timeout: time.Second}
	for {
		response, err := probe.Get(upstream.String() + "/api/v2/health/live")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("compiled HTTP server did not start")
		case <-tick.C:
		}
	}
	api := &scanHTTPSClient{t: t, client: client, origin: proxy.URL}
	api.request("POST", "/auth/login", map[string]string{"username": "owner", "password": password}, 200, nil)
	var csrf struct {
		Token string `json:"token"`
	}
	api.request("GET", "/auth/csrf", nil, 200, &csrf)
	api.csrf = csrf.Token
	var user auth.User
	headers := api.request("POST", "/admin/users", map[string]any{"username": "load-user", "password": rand.Text(), "role": "user", "active": true}, 201, &user)
	etag := headers.Get("ETag")
	if user.ID == "" || etag == "" {
		t.Fatal("administrative user creation omitted its ID or ETag")
	}
	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('view-load-library','Load',0);
 WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<99999)
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at)
 SELECT printf('view-load-%06d',i),'view-load-library','Album',0,0 FROM n`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version)
 SELECT ?,id,1,0,2 FROM albums WHERE id LIKE 'view-load-%'`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at)
 SELECT ?,id,5,0 FROM albums WHERE id LIKE 'view-load-%'`, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_last_account_journal BEFORE INSERT ON change_log
 WHEN NEW.kind='album' AND NEW.resource_id='view-load-099999'
 AND (SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id LIKE 'view-load-%')=99999
 BEGIN SELECT RAISE(ABORT,'injected late account journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	check := func(deleted bool) {
		t.Helper()
		var users, views, ratings, totals, averages, versions, events, uniqueEvents, badEvents, missing int64
		if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM users WHERE id=?),
 (SELECT COUNT(*) FROM album_views WHERE user_id=?),
 (SELECT COUNT(*) FROM ratings WHERE user_id=? AND rating IS NOT NULL),
 (SELECT SUM(view_count) FROM albums WHERE id LIKE 'view-load-%'),
 (SELECT COUNT(*) FROM albums WHERE id LIKE 'view-load-%' AND rating_avg IS NOT NULL),
 (SELECT COUNT(*) FROM albums WHERE id LIKE 'view-load-%' AND version=2),
 (SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id LIKE 'view-load-%'),
 (SELECT COUNT(DISTINCT resource_id) FROM change_log WHERE kind='album' AND resource_id LIKE 'view-load-%'),
 (SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id LIKE 'view-load-%' AND
   (json_type(payload_json,'$.view_count') IS NOT 'integer' OR json_extract(payload_json,'$.view_count')!=0 OR
    json_type(payload_json,'$.rating_avg') IS NOT 'null' OR json_type(payload_json,'$.version') IS NOT 'integer' OR
    json_extract(payload_json,'$.version')!=2 OR json_extract(payload_json,'$.id') IS NOT resource_id OR
    json_type(payload_json,'$.tag_ids') IS NOT 'array')),
 (SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE c.kind='album' AND c.resource_id LIKE 'view-load-%' AND o.id IS NULL)`, user.ID, user.ID, user.ID).Scan(&users, &views, &ratings, &totals, &averages, &versions, &events, &uniqueEvents, &badEvents, &missing); err != nil {
			t.Fatal(err)
		}
		got := [10]int64{users, views, ratings, totals, averages, versions, events, uniqueEvents, badEvents, missing}
		want := [10]int64{1, 100000, 100000, 100000, 100000, 0, 0, 0, 0, 0}
		if deleted {
			want = [10]int64{0, 0, 0, 0, 0, 100000, 100000, 100000, 0, 0}
		}
		if got != want {
			t.Fatal("account deletion lost atomic rows, aggregates, versions or complete events", got, want)
		}
	}
	remove := func(want int) time.Duration {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), "DELETE", proxy.URL+"/api/v2/admin/users/"+user.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", proxy.URL)
		req.Header.Set("X-CSRF-Token", api.csrf)
		req.Header.Set("If-Match", etag)
		start := time.Now()
		response, err := client.Do(req)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal("compiled account deletion request failed", elapsed, err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil || response.StatusCode != want {
			t.Fatalf("compiled account deletion: status=%d want=%d after=%s body=%s error=%v", response.StatusCode, want, elapsed, body, err)
		}
		if want == 500 {
			var problem map[string]any
			if json.Unmarshal(body, &problem) != nil {
				t.Fatal("late failure did not return a problem document", string(body))
			}
			delete(problem, "request_id")
			public, err := json.Marshal(problem)
			if err != nil || bytes.Contains(public, []byte("injected")) || bytes.Contains(public, []byte("SQL")) {
				t.Fatal("late failure exposed internal diagnostics", string(public), err)
			}
		}
		return elapsed
	}
	failed := remove(500)
	check(false)
	if _, err := db.Exec(`DROP TRIGGER fail_last_account_journal`); err != nil {
		t.Fatal(err)
	}
	completed := remove(204)
	if completed >= 10*time.Second {
		t.Fatal("server reported account deletion success after the metadata deadline", completed)
	}
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
	t.Log("compiled 100k account deletion: late failure", failed, "successful retry", completed)
}
