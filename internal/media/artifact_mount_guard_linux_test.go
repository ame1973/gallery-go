//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArtifactMountGuardRejectsUnobservableSourcesBeforeWrites(t *testing.T) {
	for _, scenario := range []string{"missing", "symlink", "file", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			svc, artifact, data, original := privateArtifactFixture(t)
			path := filepath.Join(filepath.Dir(svc.cacheRoot), "unobservable")
			switch scenario {
			case "symlink":
				if err := os.Symlink(filepath.Dir(original), path); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if os.Geteuid() == 0 {
					t.Skip("privileged user can bypass directory permissions")
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(path, 0o700); err != nil {
						t.Error("cannot restore owned fixture permissions", err)
					}
				})
			}
			svc.sources["unobservable"] = SourceRoot{Path: path, Marker: ".marker"}
			if err := svc.storeAndCloseArtifactForTest(t.Context(), "job", artifact, data); !errors.Is(err, ErrVariantUnavailable) {
				t.Fatal("unobservable configured source accepted", err)
			}
			entries, err := os.ReadDir(svc.cacheRoot)
			if err != nil || len(entries) != 0 {
				t.Fatal("source preflight created cache files", entries, err)
			}
			if current, err := os.ReadFile(original); err != nil || !bytes.Equal(current, data) {
				t.Fatal("source was modified", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if data, err := artifactMountData(ctx); !errors.Is(err, context.Canceled) || data != nil {
		t.Fatal("cancelled mount observation continued", data, err)
	}
}

func TestArtifactMountGuardClosesPinnedSourcesOnFailure(t *testing.T) {
	svc, artifact, data, original := privateArtifactFixture(t)
	svc.sources["missing"] = SourceRoot{Path: filepath.Join(filepath.Dir(svc.cacheRoot), "zz-missing"), Marker: ".marker"}
	attempt := func() {
		t.Helper()
		if err := svc.storeAndCloseArtifactForTest(t.Context(), "job", artifact, data); !errors.Is(err, ErrVariantUnavailable) {
			t.Fatal("unobservable source accepted", err)
		}
	}
	attempt()
	before, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		attempt()
	}
	after, err := os.ReadDir("/proc/self/fd")
	if err != nil || len(after) > len(before) {
		t.Fatal("failed source observations retained descriptors", len(before), len(after), err)
	}
	entries, err := os.ReadDir(svc.cacheRoot)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed source observations created cache files", entries, err)
	}
	if current, err := os.ReadFile(original); err != nil || !bytes.Equal(current, data) {
		t.Fatal("source fixture was changed", err)
	}
}

func TestArtifactMountGuardRechecksSourceIdentityWithoutPinningContents(t *testing.T) {
	svc, _, data, original := privateArtifactFixture(t)
	gate, err := svc.lockArtifacts(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if len(gate.mountSources) != 1 {
		t.Fatal("source directory was not pinned")
	}
	pinned := gate.mountSources[0]
	flags, err := unix.FcntlInt(pinned.Fd(), unix.F_GETFL, 0)
	fdFlags, fdErr := unix.FcntlInt(pinned.Fd(), unix.F_GETFD, 0)
	if err != nil || fdErr != nil || flags&unix.O_ACCMODE != unix.O_RDONLY || fdFlags&unix.FD_CLOEXEC == 0 {
		t.Fatal("pinned source directory is not read-only and close-on-exec", err, fdErr)
	}
	source := filepath.Dir(original)
	if err := os.WriteFile(filepath.Join(source, "new-owned-file"), []byte("test"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := gate.check(t.Context()); err != nil {
		t.Fatal("ordinary source content change invalidated its directory identity", err)
	}
	moved := source + "-moved"
	if err := os.Rename(source, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := gate.check(t.Context()); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("replacement source directory passed the mount guard", err)
	}
	if current, err := os.ReadFile(filepath.Join(moved, filepath.Base(original))); err != nil || !bytes.Equal(current, data) {
		t.Fatal("source fixture was changed", err)
	}
	if err := gate.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := pinned.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closing the gate retained the source descriptor", err)
	}
}
