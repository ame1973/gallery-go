//go:build favorite_delete_load

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/store"
)

// These helpers may signal only the server process created by the test. They
// introduce no production hook, alternate handler, lease edit or user database.
func startFavoriteLoadServer(t *testing.T, binary, config string) (stop, kill func()) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "favorite-server.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	process := exec.Command(binary, "serve", "--config", config)
	process.Stdout, process.Stderr = log, log
	if err := process.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.Wait() }()
	var once sync.Once
	finish := func(abrupt bool) {
		once.Do(func() {
			var signalErr error
			if abrupt {
				signalErr = process.Process.Kill()
			} else {
				signalErr = process.Process.Signal(syscall.SIGTERM)
			}
			if signalErr != nil {
				t.Errorf("could not signal test-owned server: %v", signalErr)
			}
			select {
			case err := <-done:
				if abrupt {
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						t.Errorf("server did not exit from SIGKILL: %v", err)
					} else if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
						t.Errorf("server did not exit from SIGKILL: %v", exit)
					}
				} else if err != nil {
					t.Errorf("server did not stop gracefully: %v", err)
				}
			case <-time.After(15 * time.Second):
				_ = process.Process.Kill()
				<-done
				t.Error("test-owned server did not stop")
			}
			log.Close()
			if t.Failed() {
				content, readErr := os.ReadFile(logPath)
				t.Logf("isolated server log (read error: %v):\n%s", readErr, content)
			}
		})
	}
	stop = func() { finish(false) }
	kill = func() { finish(true) }
	t.Cleanup(stop)
	return stop, kill
}

func waitFavoriteLoadServer(t *testing.T, origin string) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	probe := &http.Client{Timeout: time.Second}
	for {
		response, err := probe.Get(origin + "/api/v2/health/live")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("compiled HTTP server did not start")
		case <-tick.C:
		}
	}
}

func killFavoriteDeleteAfterWALSpill(t *testing.T, db *sql.DB, wal string, client *http.Client, origin, csrf, id, etag, key, token string, kill func()) {
	t.Helper()
	var busy, frames, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed); err != nil || busy != 0 {
		t.Fatal("could not establish an isolated empty WAL checkpoint", busy, frames, checkpointed, err)
	}
	if info, err := os.Stat(wal); err == nil && info.Size() != 0 {
		t.Fatal("fixture WAL was not truncated before the request", info.Size())
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	observer, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if _, err := observer.ExecContext(t.Context(), `PRAGMA busy_timeout=0`); err != nil {
		t.Fatal(err)
	}
	defer observer.ExecContext(context.Background(), `PRAGMA busy_timeout=5000`)
	body, err := json.Marshal(map[string]string{"action": "remove_favorites", "confirmation_token": token})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outcome := make(chan string, 1)
	go func() {
		request, err := http.NewRequestWithContext(ctx, "DELETE", origin+"/api/v2/favorite-collections/"+id, bytes.NewReader(body))
		if err != nil {
			outcome <- fmt.Sprintf("request: %v", err)
			return
		}
		request.Header.Set("Origin", origin)
		request.Header.Set("X-CSRF-Token", csrf)
		request.Header.Set("If-Match", etag)
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			outcome <- fmt.Sprintf("transport: %v", err)
			return
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		outcome <- fmt.Sprintf("status=%d read=%v", response.StatusCode, err)
	}()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case result := <-outcome:
			t.Fatal("deletion finished before the WAL spill checkpoint", result)
		default:
		}
		info, err := os.Stat(wal)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		// The test server has no worker or scheduler; its session update cannot
		// produce this much WAL. A spill plus a busy writer is real uncommitted work.
		if err == nil && info.Size() >= 256<<10 {
			writer, err := observer.BeginTx(t.Context(), nil)
			if store.IsBusy(err) {
				kill()
				break
			}
			if err != nil {
				t.Fatal("cannot observe the isolated SQLite writer", err)
			}
			if err := writer.Rollback(); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case result := <-outcome:
			t.Fatal("deletion finished before the writer checkpoint", result)
		case <-deadline.C:
			t.Fatal("deletion did not reach the WAL spill checkpoint")
		case <-tick.C:
		}
	}
	select {
	case result := <-outcome:
		if result == "status=204 read=<nil>" {
			t.Fatal("killed in-flight deletion reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("killed server left its HTTPS request open")
	}
}
