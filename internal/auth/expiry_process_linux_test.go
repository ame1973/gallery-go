//go:build linux

package auth

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/store"
)

type expiryProcessInput struct {
	Database string
	Token    string
	Now      int64
	Mode     string
}

type expiryChild struct {
	command *exec.Cmd
	input   io.WriteCloser
	output  *bufio.Scanner
}

func startExpiryChild(t *testing.T, input expiryProcessInput) *expiryChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestObservedExpiryProcessChild$")
	// A child race must fail immediately, not be hidden by the later expected kill.
	command.Env = append(os.Environ(), "GALLERYGO_EXPIRY_TEST_CHILD=1", "GORACE=halt_on_error=1")
	command.Stderr = os.Stderr
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	child := &expiryChild{command: command, input: stdin, output: bufio.NewScanner(stdout)}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		_ = stdin.Close()
	})
	// Temporary database path and session capability travel only through a pipe.
	if err := json.NewEncoder(stdin).Encode(input); err != nil {
		t.Fatal(err)
	}
	expectExpiryChildLine(t, child, "READY")
	return child
}

func expectExpiryChildLine(t *testing.T, child *expiryChild, want string) {
	t.Helper()
	if !child.output.Scan() || child.output.Text() != want {
		t.Fatalf("expiry child protocol: want %s, received %q, error %v", want, child.output.Text(), child.output.Err())
	}
}

func TestObservedExpiryProcessChild(t *testing.T) {
	if os.Getenv("GALLERYGO_EXPIRY_TEST_CHILD") != "1" {
		t.Skip("test-only subprocess entry point")
	}
	decoder := json.NewDecoder(os.Stdin)
	var input expiryProcessInput
	if err := decoder.Decode(&input); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), input.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := New(db, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.UnixMilli(input.Now).UTC() }
	if _, err := io.WriteString(os.Stdout, "READY\n"); err != nil {
		t.Fatal(err)
	}
	var start string
	if err := decoder.Decode(&start); err != nil || start != "START" {
		t.Fatalf("start protocol: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if input.Mode == "transaction" {
		tx, beginErr := db.BeginTx(ctx, nil)
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `INSERT INTO expiry_process_business VALUES(1)`); err != nil {
			t.Fatal(err)
		}
		_, err = s.SessionInTx(ctx, tx, Session{Token: input.Token})
		_ = tx.Rollback()
	} else if input.Mode == "authenticate" || input.Mode == "verify" {
		_, err = s.Authenticate(ctx, input.Token)
	} else {
		t.Fatal("invalid child mode")
	}
	if err == nil {
		_, _ = io.WriteString(os.Stdout, "ACCEPTED\n")
		t.Fatal("child accepted the expired session")
	}
	if !errors.Is(err, ErrUnauthenticated) {
		_, _ = io.WriteString(os.Stdout, "ERROR\n")
		t.Fatalf("child did not reject the session: %v", err)
	}
	if _, err := io.WriteString(os.Stdout, "REJECTED\n"); err != nil {
		t.Fatal(err)
	}
	if input.Mode != "verify" {
		// The parent kills only this owned child after receiving confirmed rejection.
		var finish string
		if err := decoder.Decode(&finish); err != nil {
			t.Fatal(err)
		}
		t.Fatal("observer was expected to be terminated, not released")
	}
}

func TestObservedExpiryProcessesSurviveKill(t *testing.T) {
	for _, kind := range []string{"idle", "absolute"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := testService(t)
			clock := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
			s.now = func() time.Time { return clock }
			session := login(t, s)
			expires := clock.Add(s.policy.Idle)
			if kind == "absolute" {
				expires = session.ExpiresAt
				for next := clock.Add(12 * time.Hour); next.Before(expires); next = next.Add(12 * time.Hour) {
					clock = next
					if _, err := s.Authenticate(t.Context(), session.Token); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := s.db.ExecContext(t.Context(), `CREATE TABLE expiry_process_business(value INTEGER)`); err != nil {
				t.Fatal(err)
			}
			var sequence int
			var name, path string
			if err := s.db.QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
				t.Fatal(err)
			}
			children := []*expiryChild{
				startExpiryChild(t, expiryProcessInput{path, session.Token, expires.UnixMilli(), "authenticate"}),
				startExpiryChild(t, expiryProcessInput{path, session.Token, expires.UnixMilli(), "transaction"}),
			}
			for _, child := range children {
				if err := json.NewEncoder(child.input).Encode("START"); err != nil {
					t.Fatal(err)
				}
			}
			for _, child := range children {
				expectExpiryChildLine(t, child, "REJECTED")
			}
			for _, child := range children {
				if err := child.command.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				err := child.command.Wait()
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("expected owned child SIGKILL, got %v", err)
				}
				status, ok := exit.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatalf("child did not exit from SIGKILL: %v", err)
				}
			}
			if err := s.db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var revoked, audit, changes, outbox, business int
			if err := db.QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM sessions WHERE id=? AND revoked_at=?),(SELECT COUNT(*) FROM audit_events WHERE event_type='session_expired' AND resource_id=?),(SELECT COUNT(*) FROM change_log WHERE kind='session' AND operation='delete' AND resource_id=?),(SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.kind='session' AND c.operation='delete' AND c.resource_id=?),(SELECT COUNT(*) FROM expiry_process_business)`, session.ID, expires.UnixMilli(), session.ID, session.ID, session.ID).Scan(&revoked, &audit, &changes, &outbox, &business); err != nil {
				t.Fatal(err)
			}
			if revoked != 1 || audit != 1 || changes != 1 || outbox != 1 || business != 0 {
				t.Errorf("reopened state after two process kills: revoked=%d audit=%d changes=%d outbox=%d business=%d", revoked, audit, changes, outbox, business)
			}
			fresh := startExpiryChild(t, expiryProcessInput{path, session.Token, expires.Add(-time.Millisecond).UnixMilli(), "verify"})
			if err := json.NewEncoder(fresh.input).Encode("START"); err != nil {
				t.Fatal(err)
			}
			expectExpiryChildLine(t, fresh, "REJECTED")
			if err := fresh.command.Wait(); err != nil {
				t.Fatalf("fresh process accepted the expired token after clock rollback: %v", err)
			}
		})
	}
}
