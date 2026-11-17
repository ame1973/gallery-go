package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/health"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func TestCompiledWorkerHealthcheckRejectsFutureHeartbeat(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "gallerygo")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	config := filepath.Join(directory, "config.yaml")
	database := filepath.Join(directory, "data", "gallerygo.db")
	assets := filepath.Join(directory, "web")
	if err := os.Mkdir(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("health test"), 0o400); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	upstream, err := url.Parse("http://" + address)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(upstream))
	defer proxy.Close()
	settings := map[string]any{
		"listen":     address,
		"base_url":   proxy.URL,
		"database":   database,
		"cache_root": filepath.Join(directory, "cache"),
		"web_root":   assets,
		"worker":     map[string]bool{"embedded": false, "required": true},
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(t.Context(), binary, "migrate", "--config", config).CombinedOutput(); err != nil {
		t.Fatalf("migrate temporary database: %v\n%s", err, output)
	}
	db, err := store.Open(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	queue, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.RegisterWorker(t.Context(), "future-cli-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE workers SET last_seen_at=? WHERE id='future-cli-worker'`, time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	stop := startCLIProcess(t, binary, config, "serve")
	defer stop()
	client := proxy.Client()
	client.Timeout = time.Second
	startup := &http.Client{Timeout: time.Second}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		response, err := startup.Get(upstream.String() + "/api/v2/health/live")
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
	checkHTTPS := func(want int) {
		t.Helper()
		response, err := client.Get(proxy.URL + "/api/v2/health/ready")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != want || response.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatalf("HTTPS readiness = %d, want %d", response.StatusCode, want)
		}
	}
	checkHTTPS(http.StatusServiceUnavailable)
	probe := func() ([]byte, error) {
		return exec.CommandContext(t.Context(), binary, "healthcheck", "--worker", "--config", config).CombinedOutput()
	}
	if output, err := probe(); err == nil || !strings.Contains(string(output), "no required scan worker") {
		t.Fatalf("future-only worker healthcheck: err=%v output=%s", err, output)
	}
	if output, err := exec.CommandContext(t.Context(), binary, "healthcheck", "--config", config).CombinedOutput(); err == nil || !strings.Contains(string(output), "server is not ready") {
		t.Fatalf("future-only HTTP healthcheck: err=%v output=%s", err, output)
	}
	if err := queue.RegisterWorker(t.Context(), "current-cli-worker", "worker", 1); err != nil {
		t.Fatal(err)
	}
	output, err := probe()
	if err != nil {
		t.Fatalf("current worker healthcheck: %v\n%s", err, output)
	}
	var state health.Readiness
	if err := json.Unmarshal(output, &state); err != nil || state.Status != "ok" || state.Worker != "ok" {
		t.Fatal("fresh worker did not establish readiness", state, err)
	}
	checkHTTPS(http.StatusOK)
	if output, err := exec.CommandContext(t.Context(), binary, "healthcheck", "--config", config).CombinedOutput(); err != nil {
		t.Fatalf("current HTTP healthcheck: %v\n%s", err, output)
	}
	if err := queue.StopWorker(t.Context(), "current-cli-worker"); err != nil {
		t.Fatal(err)
	}
	checkHTTPS(http.StatusServiceUnavailable)
	if output, err := probe(); err == nil || !strings.Contains(string(output), "no required scan worker") {
		t.Fatalf("stopped and future-only worker healthcheck: err=%v output=%s", err, output)
	}
}
