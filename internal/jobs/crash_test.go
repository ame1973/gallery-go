package jobs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/store"
	"gallerygo/migrations"
)

// This branch exists only in the test binary. A parent-created private marker
// prevents an accidentally inherited environment variable from opening a user DB.
func TestWorkerCrashProcessHelper(t *testing.T) {
	path, nonce := os.Getenv("GALLERYGO_CRASH_TEST_DB"), os.Getenv("GALLERYGO_CRASH_TEST_NONCE")
	if path == "" || nonce == "" {
		t.Skip("subprocess helper requires its parent's isolated test database")
	}
	marker, err := os.ReadFile(filepath.Join(filepath.Dir(path), ".worker-crash-test"))
	if err != nil || string(marker) != nonce {
		t.Fatal("missing private crash-test marker")
	}
	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := store.VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	policy := crashPolicy()
	q, err := New(db, policy)
	if err != nil {
		t.Fatal(err)
	}
	err = q.Run(t.Context(), "worker", map[string]Handler{"scan": func(ctx context.Context, lease Lease) error {
		if _, err := q.Progress(ctx, lease, Counts{Total: 3, Done: 1, Succeeded: 1}, true); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "CRASH_TEST_CLAIMED")
		<-ctx.Done()
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func crashPolicy() Policy {
	return Policy{Concurrency: 1, Lease: 3 * time.Second, Heartbeat: 500 * time.Millisecond, Poll: 100 * time.Millisecond, MaxAttempts: 2, RetryBase: 100 * time.Millisecond, RetryMax: time.Second}
}

func TestWorkerCrashReopensDatabaseAndReclaimsExactlyOneAttempt(t *testing.T) {
	q, _ := fixture(t)
	q.now, q.policy = time.Now, crashPolicy()
	job := enqueued(t, q, "process-crash", 0)
	var path string
	if err := q.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	nonce := store.NewID()
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".worker-crash-test"), []byte(nonce), 0o600); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestWorkerCrashProcessHelper$", "-test.timeout=20s")
	child.Env = append(os.Environ(), "GALLERYGO_CRASH_TEST_DB="+path, "GALLERYGO_CRASH_TEST_NONCE="+nonce)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			child.Process.Kill()
			child.Wait()
		}
	})
	ready := make(chan error, 1)
	go func() {
		lines := bufio.NewScanner(stdout)
		for lines.Scan() {
			if lines.Text() == "CRASH_TEST_CLAIMED" {
				ready <- nil
				return
			}
		}
		ready <- errors.New("worker process exited before claiming the isolated job")
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("worker process did not claim work")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = child.Wait()
	waited = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal("expected an abruptly killed worker", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("worker did not terminate through the injected SIGKILL", err)
	}
	before, err := q.Get(t.Context(), job.ID)
	if err != nil || before.Status != "running" || before.Attempts != 1 || before.Done != 1 || before.Succeeded != 1 {
		t.Fatal("crash lost committed progress", before, err)
	}
	// Reopen the file with a new connection pool, not merely another in-memory
	// Queue. No graceful cancellation or cleanup ran in the killed process.
	if err := q.db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted, err := New(db, crashPolicy())
	if err != nil {
		t.Fatal(err)
	}
	var replacement Lease
	eventually(t, func() bool {
		lease, err := restarted.Claim(t.Context(), "replacement-process", []string{"scan"})
		if errors.Is(err, ErrNoWork) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		replacement = lease
		return true
	})
	if replacement.Job.ID != job.ID || replacement.Job.Attempts != 2 || replacement.Job.Done != 3 || replacement.Job.Succeeded != 1 || replacement.Job.Cancelled != 2 {
		t.Fatal("recovery reset counters or duplicated the attempt", replacement.Job)
	}
	if _, err := restarted.Progress(t.Context(), replacement, Counts{Total: 4, Done: 4, Succeeded: 2, Cancelled: 2}, true); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Complete(t.Context(), replacement); err != nil {
		t.Fatal(err)
	}
	var attempts, unique int
	if err := db.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT attempt) FROM job_attempts WHERE job_id=?`, job.ID).Scan(&attempts, &unique); err != nil || attempts != 2 || unique != 2 {
		t.Fatal("duplicate or missing durable attempts", attempts, unique, err)
	}
	final, err := restarted.Get(t.Context(), job.ID)
	if err != nil || final.Status != "succeeded" || final.Cancelled != 2 || final.Succeeded != 2 {
		t.Fatal(final, err)
	}
}
