package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
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

// startAbruptWorker can kill only the child it creates. No production checkpoint,
// seed endpoint, alternate handler or environment backdoor participates in recovery.
func startAbruptWorker(t *testing.T, binary, config string) func() {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "abrupt-worker.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "worker", "--config", config)
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	var once sync.Once
	kill := func() {
		once.Do(func() {
			if err := command.Process.Kill(); err != nil {
				t.Error("worker exited before this test's SIGKILL", err)
			}
			select {
			case err := <-done:
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Error("worker did not report abrupt termination", err)
				} else if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Error("worker was not SIGKILLed", exit)
				}
			case <-time.After(10 * time.Second):
				t.Error("SIGKILLed worker did not exit")
			}
			log.Close()
		})
	}
	t.Cleanup(kill)
	return kill
}

func waitCLIListener(t *testing.T, address string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		response, err := client.Get("http://" + address + "/api/v2/health/live")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("compiled server did not become live")
		case <-tick.C:
		}
	}
}

func freezeCrashCheckpoint(t *testing.T, db *sql.DB, job, lastSource string, partial bool) *sql.Tx {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var status string
		var files, published int64
		if err := db.QueryRowContext(t.Context(), `SELECT status,COALESCE((SELECT file_count FROM inventory_generations WHERE job_id=j.id AND source_id=? AND attempt=1),0),(SELECT COUNT(*) FROM inventory_generations WHERE job_id=j.id AND state='published') FROM jobs j WHERE id=?`, lastSource, job).Scan(&status, &files, &published); err != nil {
			t.Fatal(err)
		}
		if status != "queued" && status != "running" {
			t.Fatal("test missed its real intermediate ingestion checkpoint", status, files, published)
		}
		if files >= 1 && ((partial && published == 1) || (!partial && published == 0)) {
			// A test-owned writer freezes further commits while the actual worker is
			// hashing a large regular input outside transactions. It never changes
			// production data/lease state, and prevents a timing-only terminal race.
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := tx.QueryRowContext(t.Context(), `SELECT status FROM jobs WHERE id=?`, job).Scan(&status); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if status != "running" {
				tx.Rollback()
				t.Fatal("worker completed before crash freeze", status)
			}
			return tx
		}
		select {
		case <-deadline.C:
			t.Fatal("native inventory did not reach the crash checkpoint", status, files, published)
		case <-tick.C:
		}
	}
}

func TestCompiledScanSIGKILLReopensAndRecoversStagingAndPartialPublication(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "gallerygo")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial-publication=%t", partial), func(t *testing.T) {
			directory := t.TempDir()
			dbPath := filepath.Join(directory, "data", "gallerygo.db")
			config := filepath.Join(directory, "config.yaml")
			count := 1
			if partial {
				count = 2
			}
			roots := make([]map[string]string, 0, count)
			originals := make([][]byte, 0, count)
			for index := 0; index < count; index++ {
				root := filepath.Join(directory, fmt.Sprintf("source-%d", index))
				if err := os.MkdirAll(filepath.Join(root, "Album"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, ".mounted"), []byte("isolated crash fixture marker"), 0o400); err != nil {
					t.Fatal(err)
				}
				var imageBytes bytes.Buffer
				if err := png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 12+index, 7))); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "Album", "photo.png"), imageBytes.Bytes(), 0o400); err != nil {
					t.Fatal(err)
				}
				roots = append(roots, map[string]string{"name": fmt.Sprintf("fixture-%d", index), "root": root, "marker": ".mounted"})
				originals = append(originals, imageBytes.Bytes())
			}
			// Sparse regular bytes make an observable hashing window with bounded disk
			// allocation and memory. They are test-owned, never an actual NAS input.
			padding := filepath.Join(roots[count-1]["root"], "zz-padding.bin")
			file, err := os.OpenFile(padding, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			const paddingBytes = int64(2 << 30)
			if err := file.Truncate(paddingBytes); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(padding, 0o400); err != nil {
				t.Fatal(err)
			}
			assets := filepath.Join(directory, "web")
			if err := os.Mkdir(assets, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<!doctype html><title>Isolated crash API</title>"), 0o400); err != nil {
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
			settings := map[string]any{"listen": address, "base_url": proxy.URL, "database": dbPath, "cache_root": filepath.Join(directory, "cache"), "web_root": assets, "sources": roots, "scheduler": map[string]bool{"enabled": false}, "scanner": map[string]int{"batch_size": 1}, "worker": map[string]any{"embedded": false, "concurrency": 1, "lease": "3s", "heartbeat": "500ms", "poll": "100ms", "retry_base": "100ms", "retry_max": "200ms"}}
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
					t.Fatalf("%v: %v\n%s", args, err, output)
				}
			}
			stopServer := startCLIProcess(t, binary, config, "serve")
			defer stopServer()
			waitCLIListener(t, address)
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
			api.request("POST", "/admin/libraries", map[string]string{"name": "Crash recovery"}, 201, &collection)
			sources := make([]library.Source, 0, count)
			for _, root := range roots {
				var source library.Source
				api.request("POST", "/admin/sources", map[string]any{"library_id": collection.ID, "root_name": root["name"], "minimum_files": 1}, 201, &source)
				sources = append(sources, source)
			}
			if partial && sources[0].ID >= sources[1].ID {
				t.Fatal("fixture source ordering is not monotonic")
			}
			var accepted jobs.Job
			api.request("POST", "/admin/scans", map[string]string{"mode": "full"}, 202, &accepted)
			monitor, err := store.Open(t.Context(), dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer monitor.Close()
			kill := startAbruptWorker(t, binary, config)
			freeze := freezeCrashCheckpoint(t, monitor, accepted.ID, sources[count-1].ID, partial)
			defer freeze.Rollback()
			queue, err := jobs.New(monitor, jobs.DefaultPolicy())
			if err != nil {
				t.Fatal(err)
			}
			before, err := queue.Get(t.Context(), accepted.ID)
			if err != nil || before.Attempts != 1 || before.TotalKnown || before.Status != "running" || before.Done == 0 {
				t.Fatal("checkpoint is not real unfinished work", before, before.Counts, err)
			}
			var owner string
			if err := freeze.QueryRow(`SELECT lease_owner FROM jobs WHERE id=?`, before.ID).Scan(&owner); err != nil {
				t.Fatal(err)
			}
			oldLease := jobs.Lease{Job: before, Owner: owner}
			var originalID, originalRevision string
			var originalVersion int64
			if partial {
				if err := freeze.QueryRow(`SELECT m.id,m.current_revision_id,m.version FROM media m JOIN source_entries e ON e.id=m.source_entry_id WHERE e.source_id=?`, sources[0].ID).Scan(&originalID, &originalRevision, &originalVersion); err != nil {
					t.Fatal(err)
				}
			}
			kill()
			if err := freeze.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := monitor.Close(); err != nil {
				t.Fatal(err)
			}
			stopServer() // Close every original app/monitor connection before reopening WAL.
			stopReopenedServer := startCLIProcess(t, binary, config, "serve")
			defer stopReopenedServer()
			waitCLIListener(t, address)
			stopReopenedWorker := startCLIProcess(t, binary, config, "worker")
			defer stopReopenedWorker()
			reopened, err := store.Open(t.Context(), dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			reopenedQueue, err := jobs.New(reopened, jobs.DefaultPolicy())
			if err != nil {
				t.Fatal(err)
			}
			reclaimedDeadline := time.NewTimer(15 * time.Second)
			defer reclaimedDeadline.Stop()
			reclaimTick := time.NewTicker(10 * time.Millisecond)
			defer reclaimTick.Stop()
			var live jobs.Job
			for {
				live, err = reopenedQueue.Get(t.Context(), accepted.ID)
				if err != nil {
					t.Fatal(err)
				}
				if live.Attempts == 2 {
					break
				}
				select {
				case <-reclaimedDeadline.C:
					t.Fatal("expired native attempt was not reclaimed", live)
				case <-reclaimTick.C:
				}
			}
			if live.Status != "running" {
				t.Fatal("recovery fence needs a live successor", live)
			}
			tx, err := reopened.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var successor string
			if err := tx.QueryRow(`SELECT lease_owner FROM jobs WHERE id=?`, live.ID).Scan(&successor); err != nil || successor == owner || successor == "" {
				t.Fatal("recovery retained killed lease token", successor, err)
			}
			_, oldErr := reopenedQueue.RequireLeaseTx(t.Context(), tx, oldLease)
			_, newErr := reopenedQueue.RequireLeaseTx(t.Context(), tx, jobs.Lease{Job: live, Owner: successor})
			tx.Rollback()
			if !errors.Is(oldErr, jobs.ErrLeaseLost) || newErr != nil {
				t.Fatal("live successor did not fence the killed owner", oldErr, newErr)
			}
			result := api.waitJob(accepted.ID)
			if result.Status != "succeeded" || result.Attempts != 2 || !result.TotalKnown || result.Done != result.Total || result.Done <= before.Done || result.Succeeded < before.Succeeded || result.Failed != 0 {
				t.Fatal("recovery erased or invented attempt outcomes", result, result.Counts, before.Counts)
			}
			var history struct {
				Items []jobs.Attempt `json:"items"`
			}
			api.request("GET", "/admin/jobs/"+accepted.ID+"/attempts", nil, 200, &history)
			if len(history.Items) != 2 || history.Items[0].Status != "failed" || history.Items[0].ErrorCode == nil || *history.Items[0].ErrorCode != "LEASE_EXPIRED" || history.Items[1].Status != "succeeded" || history.Items[0].Done != before.Done || history.Items[0].Succeeded != before.Succeeded || history.Items[0].Done+history.Items[1].Done != result.Done {
				t.Fatal("attempt recovery history is not durable", history)
			}
			var medias, revisions, entries, orphanEvents, duplicateEvents, superseded int
			if err := reopened.QueryRow(`SELECT (SELECT COUNT(*) FROM media),(SELECT COUNT(*) FROM media_revisions),(SELECT COUNT(*) FROM source_entries),(SELECT COUNT(*) FROM change_log c WHERE NOT EXISTS(SELECT 1 FROM outbox_events o WHERE o.change_seq=c.seq)),(SELECT COUNT(*) FROM (SELECT actor_scope,kind,resource_id,operation,version,COUNT(*) n FROM change_log WHERE kind IN ('media','source_entry','album','person') GROUP BY actor_scope,kind,resource_id,operation,version HAVING n>1)),(SELECT COUNT(*) FROM inventory_generations WHERE job_id=? AND state='superseded')`, accepted.ID).Scan(&medias, &revisions, &entries, &orphanEvents, &duplicateEvents, &superseded); err != nil || medias != count || revisions != count || entries != count+1 || orphanEvents != 0 || duplicateEvents != 0 || superseded != 1 {
				t.Fatal("reopened publication duplicated or lost durable state", medias, revisions, entries, orphanEvents, duplicateEvents, superseded, err)
			}
			if partial {
				var id, revision string
				var version int64
				if err := reopened.QueryRow(`SELECT m.id,m.current_revision_id,m.version FROM media m JOIN source_entries e ON e.id=m.source_entry_id WHERE e.source_id=?`, sources[0].ID).Scan(&id, &revision, &version); err != nil || id != originalID || revision != originalRevision || version != originalVersion {
					t.Fatal("retry replaced already-published logical identity", id, revision, version, err)
				}
			}
			for index, root := range roots {
				var id string
				if err := reopened.QueryRow(`SELECT m.id FROM media m JOIN source_entries e ON e.id=m.source_entry_id WHERE e.source_id=?`, sources[index].ID).Scan(&id); err != nil {
					t.Fatal(err)
				}
				var original []byte
				api.request("GET", "/media/"+id+"/original", nil, 200, &original)
				if !bytes.Equal(original, originals[index]) {
					t.Fatal("recovered original is not source-bound")
				}
				if actual, err := os.ReadFile(filepath.Join(root["root"], "Album", "photo.png")); err != nil || !bytes.Equal(actual, originals[index]) {
					t.Fatal("crash recovery wrote source bytes", err)
				}
			}
			if stat, err := os.Stat(padding); err != nil || stat.Size() != paddingBytes || stat.Mode().Perm() != 0o400 {
				t.Fatal("crash recovery modified read-only padding", stat, err)
			}
			stopReopenedWorker()
			stopReopenedServer()
		})
	}
}
