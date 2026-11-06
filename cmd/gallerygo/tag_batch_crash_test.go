package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func freezeTagBatchCheckpoint(t *testing.T, db *sql.DB, id string, attempt int) (*sql.Tx, jobs.Job, func()) {
	t.Helper()
	q, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	owned := true
	defer func() {
		if owned {
			writer.Close()
		}
	}()
	// A blocking busy handler can repeatedly miss short DB-only transaction gaps.
	// Only this test-owned pinned monitor connection uses nonblocking acquisition;
	// production worker connections keep their original busy/lease policies.
	if _, err := writer.ExecContext(t.Context(), `PRAGMA busy_timeout=0`); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		job, err := q.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != "queued" && job.Status != "running" {
			t.Fatal("batch completed before its real partial-work checkpoint", job, job.Counts)
		}
		if job.Attempts == attempt && job.Status == "running" && job.TotalKnown && job.Done > 0 && job.Done < job.Total {
			for {
				tx, err := writer.BeginTx(t.Context(), nil)
				if store.IsBusy(err) {
					select {
					case <-deadline.C:
						t.Fatal("monitor could not freeze a live transaction gap")
					default:
					}
					runtime.Gosched()
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				job, err = q.Get(t.Context(), id)
				if err != nil || job.Status != "running" || job.Attempts != attempt || !job.TotalKnown || job.Done >= job.Total {
					tx.Rollback()
					t.Fatal("worker passed unfinished checkpoint before writer freeze", job, job.Counts, err)
				}
				owned = false
				var once sync.Once
				release := func() {
					once.Do(func() {
						if err := tx.Rollback(); err != nil {
							t.Error(err)
						}
						if err := writer.Close(); err != nil {
							t.Error(err)
						}
					})
				}
				return tx, job, release
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("batch did not reach live partial-work checkpoint", job, job.Counts)
		case <-tick.C:
		}
	}
}

func TestCompiledTagBatchSIGKILLReopensWithoutRepeatingCommittedTargets(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "gallerygo")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	directory := t.TempDir()
	dbPath := filepath.Join(directory, "data", "gallerygo.db")
	config := filepath.Join(directory, "config.yaml")
	assets := filepath.Join(directory, "web")
	if err := os.Mkdir(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<!doctype html><title>Isolated batch crash API</title>"), 0o400); err != nil {
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
	settings := map[string]any{"listen": address, "base_url": proxy.URL, "database": dbPath, "cache_root": filepath.Join(directory, "cache"), "web_root": assets, "sources": []any{}, "scheduler": map[string]bool{"enabled": false}, "worker": map[string]any{"embedded": false, "concurrency": 1, "lease": "3s", "heartbeat": "500ms", "poll": "100ms", "retry_base": "100ms", "retry_max": "200ms"}}
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
	// Only this freshly migrated temporary DB is seeded before any app starts.
	// The bounded SQL work makes an unfinished writer checkpoint observable; no
	// production branch, alternate handler, user DB or lease/data rewrite is used.
	seed, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	tx, err := seed.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('batch-crash-library','Batch crash',0)`); err != nil {
		t.Fatal(err)
	}
	ids, tags := make([]string, 100), make([]string, 100)
	for index := range ids {
		ids[index] = fmt.Sprintf("batch-crash-%03d", index)
		tags[index] = fmt.Sprintf("batch-crash-tag-%03d", index)
		if _, err := tx.Exec(`INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES(?,'batch-crash-library','Crash',0,0)`, ids[index]); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO tags(id,name) VALUES(?,?)`, tags[index], tags[index]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`CREATE TRIGGER batch_test_work BEFORE INSERT ON album_tag_batch_results BEGIN SELECT (WITH RECURSIVE steps(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM steps WHERE i<100000) SELECT sum(i) FROM steps); END`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
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
	var accepted jobs.Job
	api.request("POST", "/admin/batches/tags", access.TagBatchRequest{AlbumIDs: ids, TagIDs: tags}, 202, &accepted)
	monitor, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close()
	kill := startAbruptWorker(t, binary, config)
	freeze, before, release := freezeTagBatchCheckpoint(t, monitor, accepted.ID, 1)
	defer release()
	if before.Total != 100 || before.Done < 1 || before.Done >= 100 || before.Succeeded != before.Done || before.Skipped != 0 || before.Failed != 0 {
		t.Fatal("checkpoint is not actually partially applied", before, before.Counts)
	}
	var owner string
	if err := freeze.QueryRow(`SELECT lease_owner FROM jobs WHERE id=?`, accepted.ID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	oldLease := jobs.Lease{Job: before, Owner: owner}
	committed := map[string]int64{}
	rows, err := freeze.Query(`SELECT album_id,album_version FROM album_tag_batch_results WHERE job_id=? AND outcome='applied'`, accepted.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		var version int64
		if err := rows.Scan(&id, &version); err != nil {
			t.Fatal(err)
		}
		committed[id] = version
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if int64(len(committed)) != before.Done {
		t.Fatal("committed targets/progress were not atomic", len(committed), before.Counts)
	}
	kill()
	release()
	if err := monitor.Close(); err != nil {
		t.Fatal(err)
	}
	stopServer() // Every original app, worker and DB handle is closed before reopen.
	stopReopenedServer := startCLIProcess(t, binary, config, "serve")
	defer stopReopenedServer()
	waitCLIListener(t, address)
	stopWorker := startCLIProcess(t, binary, config, "worker")
	defer stopWorker()
	reopened, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	liveFreeze, live, releaseLive := freezeTagBatchCheckpoint(t, reopened, accepted.ID, 2)
	defer releaseLive()
	var successor string
	if err := liveFreeze.QueryRow(`SELECT lease_owner FROM jobs WHERE id=?`, accepted.ID).Scan(&successor); err != nil || successor == "" || successor == owner {
		t.Fatal("successor retained killed lease", successor, err)
	}
	q, err := jobs.New(reopened, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	_, oldErr := q.RequireLeaseTx(t.Context(), liveFreeze, oldLease)
	_, newErr := q.RequireLeaseTx(t.Context(), liveFreeze, jobs.Lease{Job: live, Owner: successor})
	if !errors.Is(oldErr, jobs.ErrLeaseLost) || newErr != nil {
		t.Fatal("live successor did not fence killed owner", oldErr, newErr)
	}
	releaseLive()
	result := api.waitJob(accepted.ID)
	want := jobs.Counts{Total: 200, Done: 200, Succeeded: 100, Skipped: before.Done, Cancelled: 100 - before.Done}
	if result.Status != "succeeded" || result.Attempts != 2 || result.Counts != want {
		t.Fatal("recovery erased history or double-applied targets", result, result.Counts, want)
	}
	var history struct {
		Items []jobs.Attempt `json:"items"`
	}
	api.request("GET", "/admin/jobs/"+accepted.ID+"/attempts", nil, 200, &history)
	if len(history.Items) != 2 || history.Items[0].Status != "failed" || history.Items[0].ErrorCode == nil || *history.Items[0].ErrorCode != "LEASE_EXPIRED" || history.Items[0].Succeeded != before.Done || history.Items[0].Cancelled != 100-before.Done || history.Items[1].Status != "succeeded" || history.Items[1].Skipped != before.Done {
		t.Fatal("per-attempt recovery history lost real outcomes", history)
	}
	var resolved, links, versions, outcomes, orphan, duplicates int
	if err := reopened.QueryRow(`SELECT (SELECT COUNT(*) FROM album_tag_batch_results WHERE job_id=? AND outcome='applied'),(SELECT COUNT(*) FROM album_tags),(SELECT COUNT(*) FROM albums WHERE version=2),(SELECT COUNT(*) FROM album_tag_batch_results WHERE job_id=?),(SELECT COUNT(*) FROM change_log c WHERE NOT EXISTS(SELECT 1 FROM outbox_events o WHERE o.change_seq=c.seq)),(SELECT COUNT(*) FROM (SELECT resource_id,version,COUNT(*) n FROM change_log WHERE kind='album' GROUP BY resource_id,version HAVING n>1))`, accepted.ID, accepted.ID).Scan(&resolved, &links, &versions, &outcomes, &orphan, &duplicates); err != nil || resolved != 100 || links != 10000 || versions != 100 || int64(outcomes) != 100+before.Done || orphan != 0 || duplicates != 0 {
		t.Fatal("reopened business/result/event state is inconsistent", resolved, links, versions, outcomes, orphan, duplicates, err)
	}
	for id, version := range committed {
		var current int64
		if err := reopened.QueryRow(`SELECT version FROM albums WHERE id=?`, id).Scan(&current); err != nil || current != version {
			t.Fatal("recovery rewrote an already committed target", id, current, version, err)
		}
	}
	var seen int
	for cursor := ""; ; {
		var page struct {
			Items      []access.TagBatchResult `json:"items"`
			NextCursor *string                 `json:"next_cursor"`
		}
		path := "/admin/batches/" + accepted.ID + "/results?limit=100"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		api.request("GET", path, nil, 200, &page)
		seen += len(page.Items)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if int64(seen) != 100+before.Done {
		t.Fatal("result pages lost repeated-attempt history", seen, before.Done)
	}
	stopWorker()
	stopReopenedServer()
}
