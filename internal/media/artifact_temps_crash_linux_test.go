//go:build linux && (amd64 || arm64)

package media

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

func artifactTempsCrashPolicy() jobs.Policy {
	return jobs.Policy{Concurrency: 1, Lease: 3 * time.Second, Heartbeat: 500 * time.Millisecond, Poll: 100 * time.Millisecond, MaxAttempts: 2, RetryBase: 100 * time.Millisecond, RetryMax: time.Second}
}

// The nonce marker is checked before any database/cache open, so inherited test
// environment variables cannot select an unrelated user's database or cache.
func TestArtifactTempsCrashProcessHelper(t *testing.T) {
	database, cache, nonce := os.Getenv("GALLERYGO_TEMP_TEST_DB"), os.Getenv("GALLERYGO_TEMP_TEST_CACHE"), os.Getenv("GALLERYGO_TEMP_TEST_NONCE")
	if database == "" || cache == "" || nonce == "" {
		t.Skip("requires parent-owned isolated crash-test storage")
	}
	marker, err := os.ReadFile(filepath.Join(filepath.Dir(database), ".artifact-temps-test"))
	if err != nil || string(marker) != nonce+"\n"+cache {
		t.Fatal("missing parent-owned temporary-artifact test marker")
	}
	db, err := store.Open(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(db, artifactTempsCrashPolicy())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := queue.Claim(t.Context(), "owned-artifact-temp-child", []string{"reconcile_temps"})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "ARTIFACT_TEMPS_CLAIMED:"+nonce)
	svc := New(db, nil, cache, nil)
	if _, err := svc.reconcileArtifactTemps(t.Context(), queue, lease, artifactTempRequest{JobLimit: 100, EntryLimit: 1000}); err != nil {
		t.Fatal(err)
	}
}

func freezeArtifactTempsCheckpoint(t *testing.T, db *sql.DB, id, directory string) (func(), string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	connection, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	observed := 100
	for {
		// Observe without a writer first. Repeatedly grabbing the writer before
		// cleanup starts can starve its default busy handler and invent a stall.
		entries, err := os.ReadDir(directory)
		if err != nil {
			connection.Close()
			t.Fatal(err)
		}
		observed = len(entries)
		if observed >= 25 && observed <= 75 {
			tx, err := connection.BeginTx(ctx, nil)
			if err != nil && !store.IsBusy(err) {
				connection.Close()
				t.Fatal(err)
			}
			if err == nil {
				var status, owner string
				var total, done int64
				if err := tx.QueryRowContext(ctx, `SELECT status,total,done,COALESCE(lease_owner,'') FROM jobs WHERE id=?`, id).Scan(&status, &total, &done, &owner); err != nil {
					tx.Rollback()
					connection.Close()
					t.Fatal(err)
				}
				// Only the owned fixture freeze observes files inside a writer.
				entries, err := os.ReadDir(directory)
				if err != nil {
					tx.Rollback()
					connection.Close()
					t.Fatal(err)
				}
				if status == "running" && total == 1 && done == 0 && len(entries) >= 25 && len(entries) <= 75 {
					var once sync.Once
					return func() { once.Do(func() { tx.Rollback(); connection.Close() }) }, owner, len(entries)
				}
				tx.Rollback()
			}
		}
		select {
		case <-ctx.Done():
			connection.Close()
			t.Fatal("could not freeze a partial private cleanup", observed, ctx.Err())
		case <-tick.C:
		}
	}
}

func TestArtifactTempsSIGKILLReopensWithCancelledUnconfirmedScopeAndFreshBudget(t *testing.T) {
	f := artifactTempsFixture(t, true)
	for range 100 {
		writeArtifactTemp(t, f, f.lease.Job.ID, "", 25*time.Hour)
	}
	queue, err := jobs.New(f.db, artifactTempsCrashPolicy())
	if err != nil {
		t.Fatal(err)
	}
	f.queue = queue
	request := artifactTempRequest{JobLimit: 100, EntryLimit: 1000}
	job := enqueueArtifactTemps(t, f, request)
	// Bounded fixture-only SQL stretches real per-file decision transactions so
	// the parent can observe a mid-scope checkpoint without production hooks.
	if _, err := f.db.Exec(fmt.Sprintf(`CREATE TRIGGER temp_test_decision_work AFTER UPDATE OF heartbeat_at ON jobs WHEN NEW.id='%s' BEGIN SELECT (WITH RECURSIVE work(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM work WHERE n<5000) SELECT sum(n) FROM work); END`, job.ID)); err != nil {
		t.Fatal(err)
	}
	var database string
	if err := f.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	nonce := store.NewID()
	if err := os.WriteFile(filepath.Join(filepath.Dir(database), ".artifact-temps-test"), []byte(nonce+"\n"+f.svc.cacheRoot), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestArtifactTempsCrashProcessHelper$", "-test.timeout=18s")
	command.Env = append(os.Environ(), "GALLERYGO_TEMP_TEST_DB="+database, "GALLERYGO_TEMP_TEST_CACHE="+f.svc.cacheRoot, "GALLERYGO_TEMP_TEST_NONCE="+nonce)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			command.Process.Kill()
			command.Wait()
		}
		if t.Failed() {
			t.Log("owned cleanup child stderr:", stderr.String())
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ARTIFACT_TEMPS_CLAIMED:"+nonce+"\n" {
		command.Process.Kill()
		command.Wait()
		waited = true
		t.Fatal("owned cleanup child did not claim its private job", line, err, stderr.String())
	}
	directory := filepath.Join(f.svc.cacheRoot, "tmp", f.lease.Job.ID)
	release, killedOwner, remaining := freezeArtifactTempsCheckpoint(t, f.db, job.ID, directory)
	defer release()
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	waited = true
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if err == nil || !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("cleanup did not terminate through actual owned SIGKILL", err)
	}
	release()
	if remaining < 25 || remaining > 75 {
		t.Fatal("fixture did not interrupt partially unlinked files", remaining)
	}
	// Close every original connection/process before reopening. Do not rewrite
	// source, job data, lease timestamps or owner tokens to manufacture recovery.
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := store.VerifySchema(t.Context(), reopened, migrations.Files); err != nil {
		t.Fatal(err)
	}
	restarted, err := jobs.New(reopened, artifactTempsCrashPolicy())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var successor jobs.Lease
	for {
		successor, err = restarted.Claim(t.Context(), "private-temp-successor", []string{"reconcile_temps"})
		if err == nil {
			break
		}
		if !errors.Is(err, jobs.ErrNoWork) {
			t.Fatal(err)
		}
		select {
		case <-deadline.C:
			t.Fatal("real cleanup lease expiry was not recovered")
		case <-tick.C:
		}
	}
	if successor.Owner == killedOwner || successor.Job.ID != job.ID || successor.Job.Attempts != 2 || successor.Job.Total != 1 || successor.Job.Done != 1 || successor.Job.Cancelled != 1 || successor.Job.Succeeded != 0 {
		t.Fatal("recovery claimed unconfirmed unlinks as success or reset history", successor.Job)
	}
	old := jobs.Lease{Job: jobs.Job{ID: job.ID, Attempts: 1}, Owner: killedOwner}
	svc := New(reopened, nil, f.svc.cacheRoot, f.svc.sources)
	if _, err := svc.reconcileArtifactTemps(t.Context(), restarted, old, request); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("killed owner could resume destructive work", err)
	}
	report, err := svc.reconcileArtifactTemps(t.Context(), restarted, successor, request)
	if err != nil || report.unlinked < 1 || report.unlinked > 75 || report.scopes != 1 {
		t.Fatal("successor did not reconcile the remaining private files", report, err)
	}
	final, err := restarted.Get(t.Context(), job.ID)
	if err != nil || final.Status != "succeeded" || final.Total != 2 || final.Done != 2 || final.Succeeded != 1 || final.Cancelled != 1 || final.Failed != 0 {
		t.Fatal("cumulative attempt scope counts are not truthful", final, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("reopened cleanup lost its shared directory or left eligible temp files", entries, err)
	}
	var attempts, expired, orphan, duplicate int
	if err := reopened.QueryRow(`SELECT (SELECT COUNT(*) FROM job_attempts WHERE job_id=?),(SELECT COUNT(*) FROM job_attempts WHERE job_id=? AND attempt=1 AND error_code='LEASE_EXPIRED' AND json_extract(counts_json,'$.cancelled')=1),(SELECT COUNT(*) FROM change_log c WHERE NOT EXISTS(SELECT 1 FROM outbox_events o WHERE o.change_seq=c.seq)),(SELECT COUNT(*) FROM (SELECT resource_id,version,COUNT(*) n FROM change_log WHERE kind='job' GROUP BY resource_id,version HAVING n>1))`, job.ID, job.ID).Scan(&attempts, &expired, &orphan, &duplicate); err != nil || attempts != 2 || expired != 1 || orphan != 0 || duplicate != 0 {
		t.Fatal("crash recovery lost or duplicated attempt/event history", attempts, expired, orphan, duplicate, err)
	}
	if data, err := os.ReadFile(f.file); err != nil || !bytes.Equal(data, f.data) {
		t.Fatal("crash recovery changed read-only source bytes", err)
	}
	if _, err := os.Stat(filepath.Join(f.svc.cacheRoot, artifactGateName)); err != nil {
		t.Fatal("crash recovery removed the permanent gate", err)
	}
}
