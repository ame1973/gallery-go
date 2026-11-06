//go:build linux && (amd64 || arm64)

package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func requireArtifactGateBlocked(t *testing.T, svc *Service, exclusive bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	gate, err := svc.lockArtifacts(ctx, exclusive)
	if gate != nil {
		gate.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) || gate != nil {
		t.Fatal("incompatible artifact gate was not excluded", exclusive, err)
	}
}

func TestStoredArtifactRetainsSharedExclusionUntilClosed(t *testing.T) {
	svc, artifact, data, original := privateArtifactFixture(t)
	stored, err := svc.storeStillArtifact(t.Context(), "held-job", artifact, data)
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	if stored.artifact != artifact || stored.jobID != "held-job" || stored.gate.exclusive {
		t.Fatal("storage lost its private identity/shared gate")
	}
	info, err := os.Stat(filepath.Join(svc.cacheRoot, artifactGateName))
	if err != nil || !privateArtifactGateFile(info) {
		t.Fatal("gate inode is not regular/private/single-link/empty", err)
	}
	flags, err := unix.FcntlInt(stored.gate.file.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("gate descriptor could leak to a codec exec", err)
	}
	encoded, _ := json.Marshal(stored)
	if string(encoded) != "{}" || strings.Contains(fmt.Sprint(stored), svc.cacheRoot) {
		t.Fatal("private gate/storage identity serialized", string(encoded))
	}
	shared, err := svc.lockArtifacts(t.Context(), false)
	if err != nil {
		t.Fatal("independent shared writers should coexist", err)
	}
	defer shared.Close()
	requireArtifactGateBlocked(t, svc, true)
	if err := stored.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stored.gate.check(t.Context()); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("closed storage gate was reusable", err)
	}
	requireArtifactGateBlocked(t, svc, true)
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	exclusive, err := svc.lockArtifacts(t.Context(), true)
	if err != nil {
		t.Fatal("all shared holders closed but exclusive gate is unavailable", err)
	}
	defer exclusive.Close()
	requireArtifactGateBlocked(t, svc, false)
	requireArtifactGateBlocked(t, svc, true)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if held, err := svc.storeStillArtifact(ctx, "excluded-job", artifact, data); !errors.Is(err, context.DeadlineExceeded) || held != nil {
		if held != nil {
			held.Close()
		}
		t.Fatal("storage wrote through exclusive reconciliation exclusion", err)
	}
	if _, err := os.Stat(filepath.Join(svc.cacheRoot, "tmp", "excluded-job")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("blocked storage created its job directory", err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(svc.cacheRoot, artifactGateName))
	if err != nil || !os.SameFile(info, after) {
		t.Fatal("closing replaced/unlinked the permanent gate", err)
	}
	if err := svc.storeAndCloseArtifactForTest(t.Context(), "retry-job", artifact, data); err != nil {
		t.Fatal("storage did not resume after exclusion closed", err)
	}
	if actual, err := os.ReadFile(original); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("gate changed read-only source bytes", err)
	}
	requireNoArtifactTemps(t, svc.cacheRoot)
}

func TestArtifactGateStorageFailureReleasesExclusionWithoutRepairingBytes(t *testing.T) {
	svc, artifact, data, _ := privateArtifactFixture(t)
	name := filepath.Join(svc.cacheRoot, filepath.FromSlash(artifact.storage))
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	poison := bytes.Repeat([]byte{0x7f}, len(data))
	if err := os.WriteFile(name, poison, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := svc.storeStillArtifact(t.Context(), "failed-storage", artifact, data)
	if !errors.Is(err, ErrVariantUnavailable) || stored != nil {
		if stored != nil {
			stored.Close()
		}
		t.Fatal("conflicting bytes became private storage evidence", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	gate, err := svc.lockArtifacts(ctx, true)
	if err != nil {
		t.Fatal("failed staging leaked its shared exclusion", err)
	}
	defer gate.Close()
	after, err := os.Stat(name)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("failure replaced the existing file", err)
	}
	if actual, err := os.ReadFile(name); err != nil || !bytes.Equal(actual, poison) {
		t.Fatal("failed storage repaired corrupt bytes without authority", err)
	}
	requireNoArtifactTemps(t, svc.cacheRoot)
}

func TestArtifactGateSupportsOnlyExplicitLocalFilesystemsAndCancelsBeforeIO(t *testing.T) {
	for _, kind := range []int64{unix.EXT4_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.TMPFS_MAGIC, unix.OVERLAYFS_SUPER_MAGIC} {
		if !localArtifactFilesystem(kind) {
			t.Fatal("supported local filesystem was refused", kind)
		}
	}
	// Predicate coverage, not a claim of testing actual NFS/SMB/FUSE/9P devices.
	for _, kind := range []int64{0, -1, 0x6969, 0xff534d42, 0x65735546, 0x01021997} {
		if localArtifactFilesystem(kind) {
			t.Fatal("unknown/network filesystem selected a weaker lock protocol", kind)
		}
	}
	cache := t.TempDir()
	svc := New(nil, nil, cache, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, exclusive := range []bool{false, true} {
		if gate, err := svc.lockArtifacts(ctx, exclusive); !errors.Is(err, context.Canceled) || gate != nil {
			t.Fatal("cancelled gate acquisition continued", err)
		}
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Fatal("cancelled acquisition created gate/cache bytes", entries, err)
	}
}

func TestArtifactGateRefusesUnownedLinksTypesPermissionsAndContents(t *testing.T) {
	for _, name := range []string{"symlink", "hardlink", "directory", "fifo", "permissions", "contents"} {
		t.Run(name, func(t *testing.T) {
			svc, artifact, data, original := privateArtifactFixture(t)
			gatePath := filepath.Join(svc.cacheRoot, artifactGateName)
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "symlink":
				if err := os.Symlink(outside, gatePath); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(outside, gatePath); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(gatePath, 0o700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(gatePath, 0o600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.WriteFile(gatePath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(gatePath, 0o666); err != nil {
					t.Fatal(err)
				}
			case "contents":
				if err := os.WriteFile(gatePath, []byte("unowned data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(gatePath)
			if err != nil {
				t.Fatal(err)
			}
			for _, exclusive := range []bool{false, true} {
				if gate, err := svc.lockArtifacts(t.Context(), exclusive); !errors.Is(err, ErrVariantUnavailable) || gate != nil {
					if gate != nil {
						gate.Close()
					}
					t.Fatal("unowned gate was adopted", name, err)
				}
			}
			if held, err := svc.storeStillArtifact(t.Context(), "bad-gate-job", artifact, data); !errors.Is(err, ErrVariantUnavailable) || held != nil {
				if held != nil {
					held.Close()
				}
				t.Fatal("storage bypassed an invalid coordination file", err)
			}
			after, err := os.Lstat(gatePath)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size() {
				t.Fatal("rejected gate was removed/replaced/repaired", err)
			}
			if actual, err := os.ReadFile(outside); err != nil || len(actual) != 0 {
				t.Fatal("gate rejection modified the linked outside file", err)
			}
			if actual, err := os.ReadFile(original); err != nil || !bytes.Equal(actual, data) {
				t.Fatal("invalid gate touched source", err)
			}
			if _, err := os.Stat(filepath.Join(svc.cacheRoot, "tmp")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid gate created private output directories", err)
			}
		})
	}
}

func TestArtifactGateDetectsLateRootOrNamedInodeChanges(t *testing.T) {
	for _, name := range []string{"root", "inode", "permissions", "hardlink"} {
		t.Run(name, func(t *testing.T) {
			svc, _, _, _ := privateArtifactFixture(t)
			gate, err := svc.lockArtifacts(t.Context(), false)
			if err != nil {
				t.Fatal(err)
			}
			defer gate.Close()
			file := filepath.Join(svc.cacheRoot, artifactGateName)
			switch name {
			case "root":
				if err := os.Rename(svc.cacheRoot, svc.cacheRoot+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(svc.cacheRoot, 0o700); err != nil {
					t.Fatal(err)
				}
			case "inode":
				if err := os.Rename(file, file+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.Chmod(file, 0o666); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(file, filepath.Join(t.TempDir(), "extra-link")); err != nil {
					t.Fatal(err)
				}
			}
			if err := gate.check(t.Context()); !errors.Is(err, ErrVariantUnavailable) {
				t.Fatal("changed cache/gate identity was silently adopted", err)
			}
		})
	}
}

func TestArtifactGateWaitDoesNotHoldSQLiteWriterOrPublishReady(t *testing.T) {
	f := fixture(t)
	gate, err := f.svc.lockArtifacts(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	before := inputMutationCounts(t, f.db)
	data := webpFixture(t, "lossless")
	artifact, err := validateStillArtifact(t.Context(), "gate-unpublished", "thumb_320", stillSource(3, 2, nil), data)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		stored, err := f.svc.storeStillArtifact(ctx, "waiting-job", artifact, data)
		if stored != nil {
			stored.Close()
		}
		done <- err
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if target == filepath.Join(f.cache, artifactGateName) {
				count++
			}
		}
		if count >= 2 {
			break
		}
		select {
		case err := <-done:
			t.Fatal("storage did not wait for exclusive exclusion", err)
		case <-deadline.C:
			t.Fatal("waiting gate descriptor was not opened")
		case <-tick.C:
		}
	}
	writerCtx, writerCancel := context.WithTimeout(t.Context(), time.Second)
	defer writerCancel()
	tx, err := f.db.BeginTx(writerCtx, nil)
	if err != nil {
		t.Fatal("gate contention held the SQLite writer", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("gate acquisition ignored cancellation", err)
		}
	case <-deadline.C:
		t.Fatal("cancelled gate waiter did not return")
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	stored, err := f.svc.storeStillArtifact(t.Context(), "unpublished-job", artifact, data)
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	if inputMutationCounts(t, f.db) != before {
		t.Fatal("gate/storage changed READY/business/progress/event/audit state")
	}
	variants, err := f.svc.Variants(t.Context(), f.reader, f.mediaID, "", 30)
	if err != nil || len(variants) != 1 || variants[0].ID != "variant" {
		t.Fatal("a storage lock became download authority", variants, err)
	}
}

func TestArtifactGateProcessHelper(t *testing.T) {
	cache, nonce := os.Getenv("GALLERYGO_ARTIFACT_GATE_TEST_ROOT"), os.Getenv("GALLERYGO_ARTIFACT_GATE_TEST_NONCE")
	if cache == "" || nonce == "" {
		t.Skip("only the owning test launches this helper")
	}
	svc := New(nil, nil, cache, nil)
	data := webpFixture(t, "lossless")
	artifact, err := validateStillArtifact(t.Context(), "gate-process-revision", "thumb_320", stillSource(3, 2, nil), data)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := svc.storeStillArtifact(t.Context(), "gate-process-job", artifact, data)
	if err != nil {
		t.Fatal(err)
	}
	defer stored.Close()
	fmt.Fprintln(os.Stdout, "GALLERYGO_ARTIFACT_GATE_LOCKED:"+nonce)
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactGateExcludesOtherProcessAndReleasesAfterOwnedSIGKILL(t *testing.T) {
	svc, _, _, _ := privateArtifactFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestArtifactGateProcessHelper$", "-test.timeout=12s")
	nonce := filepath.Base(t.TempDir())
	command.Env = append(os.Environ(), "GALLERYGO_ARTIFACT_GATE_TEST_ROOT="+svc.cacheRoot, "GALLERYGO_ARTIFACT_GATE_TEST_NONCE="+nonce)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
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
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "GALLERYGO_ARTIFACT_GATE_LOCKED:"+nonce+"\n" {
		command.Process.Kill()
		command.Wait()
		waited = true
		t.Fatal("owned storage child did not hold its completed artifact", line, err, stderr.String())
	}
	requireArtifactGateBlocked(t, svc, true)
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	waited = true
	status, ok := command.ProcessState.Sys().(syscall.WaitStatus)
	if err == nil || !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("owned gate holder did not terminate through SIGKILL", err)
	}
	acquireCtx, acquireCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer acquireCancel()
	exclusive, err := svc.lockArtifacts(acquireCtx, true)
	if err != nil {
		t.Fatal("killed process retained an inherited/leaked gate", err)
	}
	defer exclusive.Close()
	data := webpFixture(t, "lossless")
	artifact, err := validateStillArtifact(t.Context(), "gate-process-revision", "thumb_320", stillSource(3, 2, nil), data)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(svc.cacheRoot, filepath.FromSlash(artifact.storage)))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("interrupted storage was not a complete private artifact", err)
	}
	if _, err := os.Stat(filepath.Join(svc.cacheRoot, artifactGateName)); err != nil {
		t.Fatal("gate was removed instead of releasing its open description", err)
	}
}
