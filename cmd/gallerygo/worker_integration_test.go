package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
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
	"sync"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/store"
)

func startCLIProcess(t *testing.T, binary, config, command string) func() {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), command+".log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command(binary, command, "--config", config)
	process.Stdout, process.Stderr = log, log
	if err := process.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = process.Process.Signal(syscall.SIGTERM)
			select {
			case err := <-done:
				if err != nil {
					data, _ := os.ReadFile(logPath)
					t.Errorf("%s process failed: %v\n%s", command, err, data)
				}
			case <-time.After(15 * time.Second):
				_ = process.Process.Kill()
				<-done
				t.Errorf("%s process did not stop gracefully", command)
			}
			log.Close()
		})
	}
	t.Cleanup(stop)
	return stop
}

type scanHTTPSClient struct {
	t            *testing.T
	client       *http.Client
	origin, csrf string
}

func (c *scanHTTPSClient) request(method, path string, body any, want int, result any) http.Header {
	c.t.Helper()
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		input = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(c.t.Context(), method, c.origin+"/api/v2"+path, input)
	if err != nil {
		c.t.Fatal(err)
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", c.origin)
		req.Header.Set("Idempotency-Key", store.NewID())
		if c.csrf != "" {
			req.Header.Set("X-CSRF-Token", c.csrf)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	response, err := c.client.Do(req)
	if err != nil {
		c.t.Fatal(method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != want {
		c.t.Fatalf("%s %s: status=%d want=%d body=%s error=%v", method, path, response.StatusCode, want, data, err)
	}
	if result != nil {
		if raw, ok := result.(*[]byte); ok {
			*raw = data
		} else if err := json.Unmarshal(data, result); err != nil {
			c.t.Fatal(err)
		}
	}
	return response.Header
}

func (c *scanHTTPSClient) waitJob(id string) jobs.Job {
	c.t.Helper()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		var job jobs.Job
		c.request("GET", "/admin/jobs/"+id, nil, 200, &job)
		if job.Status != "queued" && job.Status != "running" {
			return job
		}
		select {
		case <-deadline.C:
			c.t.Fatal("compiled scan did not settle", job, job.Counts)
		case <-tick.C:
		}
	}
}

func TestCompiledCLIHTTPSNativeIngestionInEmbeddedAndStandaloneModes(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "gallerygo")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	for _, mode := range []struct{ embedded, scheduled bool }{{true, false}, {false, false}, {true, true}, {false, true}} {
		embedded, scheduled := mode.embedded, mode.scheduled
		t.Run(fmt.Sprintf("embedded=%t/scheduled=%t", embedded, scheduled), func(t *testing.T) {
			directory := t.TempDir()
			root := filepath.Join(directory, "source")
			if err := os.MkdirAll(filepath.Join(root, "Album"), 0o700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, ".mounted")
			if err := os.WriteFile(marker, []byte("isolated scan marker"), 0o400); err != nil {
				t.Fatal(err)
			}
			var input bytes.Buffer
			if err := png.Encode(&input, image.NewNRGBA(image.Rect(0, 0, 12, 7))); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(root, "Album", "photo.png")
			if err := os.WriteFile(name, input.Bytes(), 0o400); err != nil {
				t.Fatal(err)
			}
			assets := filepath.Join(directory, "web")
			if err := os.Mkdir(assets, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<!doctype html><title>Isolated API integration</title>"), 0o400); err != nil {
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
			client.Timeout = 5 * time.Second
			client.Jar, err = cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			settings := map[string]any{
				"listen": address, "base_url": proxy.URL, "database": filepath.Join(directory, "data", "gallerygo.db"),
				"cache_root": filepath.Join(directory, "cache"), "web_root": assets,
				"sources":   []map[string]string{{"name": "fixture", "root": root, "marker": ".mounted"}},
				"worker":    map[string]any{"embedded": embedded, "concurrency": 1, "poll": "100ms", "max_attempts": 2, "retry_base": "100ms", "retry_max": "200ms"},
				"scheduler": map[string]any{"enabled": scheduled},
			}
			config := filepath.Join(directory, "config.yaml")
			write := func(path string) {
				t.Helper()
				data, err := json.Marshal(settings)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write(config)
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
			stopServer := startCLIProcess(t, binary, config, "serve")
			defer stopServer()
			deadline := time.NewTimer(10 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(25 * time.Millisecond)
			defer tick.Stop()
			probe := &http.Client{Timeout: time.Second}
			for {
				response, err := probe.Get(upstream.String() + "/api/v2/health/live")
				if err == nil {
					response.Body.Close()
					if response.StatusCode == 200 {
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
			var collection struct {
				ID string `json:"id"`
			}
			api.request("POST", "/admin/libraries", map[string]string{"name": "Isolated native ingestion"}, 201, &collection)
			var source library.Source
			api.request("POST", "/admin/sources", map[string]any{"library_id": collection.ID, "root_name": "fixture", "minimum_files": 1}, 201, &source)
			var accepted jobs.Job
			if !scheduled {
				api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &accepted)
			}
			var stopWorker func()
			if !embedded {
				if !scheduled {
					var queued jobs.Job
					api.request("GET", "/admin/jobs/"+accepted.ID, nil, 200, &queued)
					if queued.Status != "queued" || queued.Attempts != 0 {
						t.Fatal("HTTP-only server consumed scan", queued)
					}
				}
				// The worker uses the same declared roots/DB but has no web assets and
				// must not try to bind the already occupied HTTP listener.
				settings["web_root"] = filepath.Join(directory, "absent-worker-assets")
				workerConfig := filepath.Join(directory, "worker.yaml")
				write(workerConfig)
				stopWorker = startCLIProcess(t, binary, workerConfig, "worker")
				defer stopWorker()
			}
			if scheduled {
				automaticDeadline := time.NewTimer(10 * time.Second)
				defer automaticDeadline.Stop()
				for accepted.ID == "" {
					var history struct {
						Items []jobs.Job `json:"items"`
					}
					api.request("GET", "/admin/jobs", nil, 200, &history)
					if len(history.Items) == 1 {
						accepted = history.Items[0]
						break
					}
					select {
					case <-automaticDeadline.C:
						t.Fatal("source policy did not create a scheduled scan")
					case <-tick.C:
					}
				}
			}
			initialID := accepted.ID
			result := api.waitJob(accepted.ID)
			if result.Status != "succeeded" || result.Attempts != 1 || result.Succeeded != 3 || result.Done != 3 || !result.TotalKnown {
				t.Fatal("compiled native execution failed", result, result.Counts)
			}
			var albums struct {
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			}
			api.request("GET", "/albums", nil, 200, &albums)
			if len(albums.Items) != 1 {
				t.Fatal("scan did not expose exactly one authorized album", albums)
			}
			var medias struct {
				Items []struct {
					ID            string `json:"id"`
					Width, Height *int64
				} `json:"items"`
			}
			api.request("GET", "/albums/"+albums.Items[0].ID+"/media", nil, 200, &medias)
			if len(medias.Items) != 1 || medias.Items[0].Width == nil || *medias.Items[0].Width != 12 || medias.Items[0].Height == nil || *medias.Items[0].Height != 7 {
				t.Fatal("native metadata was not indexed", medias)
			}
			id := medias.Items[0].ID
			var original []byte
			headers := api.request("GET", "/media/"+id+"/original", nil, 200, &original)
			if !bytes.Equal(original, input.Bytes()) || headers.Get("Content-Type") != "image/png" || headers.Get("Cache-Control") != "private, no-store" {
				t.Fatal("actual protected original changed", headers)
			}
			api.request("POST", "/admin/scans", map[string]string{"mode": "incremental", "source_id": source.ID}, 202, &accepted)
			if job := api.waitJob(accepted.ID); job.Status != "succeeded" {
				t.Fatal(job, job.Counts)
			}
			api.request("GET", "/albums/"+albums.Items[0].ID+"/media", nil, 200, &medias)
			if len(medias.Items) != 1 || medias.Items[0].ID != id {
				t.Fatal("repeat execution duplicated logical media", medias)
			}
			api.request("GET", "/admin/sources/"+source.ID, nil, 200, &source)
			generation := source.ScanGeneration
			if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			api.request("POST", "/admin/scans", map[string]string{"mode": "full", "source_id": source.ID}, 202, &accepted)
			if job := api.waitJob(accepted.ID); job.Status != "dead" || job.Attempts != 2 || job.Failed != 2 {
				t.Fatal("source outage was hidden", job, job.Counts)
			}
			api.request("GET", "/admin/sources/"+source.ID, nil, 200, &source)
			if source.ScanGeneration != generation || source.State != "unavailable" {
				t.Fatal("source outage blessed old inventory", source)
			}
			api.request("GET", "/albums/"+albums.Items[0].ID+"/media", nil, 200, &medias)
			if len(medias.Items) != 1 || medias.Items[0].ID != id {
				t.Fatal("source outage purged old index", medias)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("process recreated missing mount marker", err)
			}
			if data, err := os.ReadFile(name); err != nil || !bytes.Equal(data, input.Bytes()) {
				t.Fatal("process wrote source file", err)
			}
			if stopWorker != nil {
				stopWorker()
			}
			stopServer()
			db, err := store.Open(t.Context(), settings["database"].(string))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if scheduled {
				var actor *string
				var mode string
				if err := db.QueryRow(`SELECT created_by_user_id,json_extract(payload_json,'$.mode') FROM jobs WHERE id=?`, initialID).Scan(&actor, &mode); err != nil || actor != nil || mode != "full" {
					t.Fatal("automatic work used credentials or skipped initial full mode", actor, mode, err)
				}
			}
			role := "worker"
			if embedded {
				role = "serve"
			}
			var stopped, running, orphanEvents int
			if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM workers WHERE role=? AND state='stopped'),(SELECT COUNT(*) FROM workers WHERE state='running'),(SELECT COUNT(*) FROM change_log c WHERE NOT EXISTS(SELECT 1 FROM outbox_events o WHERE o.change_seq=c.seq))`, role).Scan(&stopped, &running, &orphanEvents); err != nil || stopped != 1 || running != 0 || orphanEvents != 0 {
				t.Fatal("process shutdown or transactional events incomplete", stopped, running, orphanEvents, err)
			}
		})
	}
}
