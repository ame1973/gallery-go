//go:build linux && (amd64 || arm64)

package media

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func testSourceSnapshot(root string) (map[string]string, error) {
	result := make(map[string]string)
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			value += fmt.Sprintf(":%x", sha256.Sum256(data))
		}
		result[relative] = value
		return nil
	})
	return result, err
}

func TestArtifactMountGuardRejectsTopologyChanges(t *testing.T) {
	host := os.Getenv("GALLERYGO_HOST_MOUNT_NAMESPACE")
	if host == "" {
		t.Skip("isolated user and mount namespace not requested")
	}
	current, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil || current == host || os.Geteuid() != 0 {
		t.Fatal("test requires a separate owned user and mount namespace", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal("cannot make the isolated namespace private", err)
	}
	svc, _, data, original := privateArtifactFixture(t)
	gate, err := svc.lockArtifacts(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	base := t.TempDir()
	from, to := filepath.Join(base, "from"), filepath.Join(base, "to")
	for _, path := range []string{from, to} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mount(from, to, "", unix.MS_BIND, ""); err != nil {
		t.Fatal("cannot create isolated topology change", err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(to, unix.MNT_DETACH); err != nil {
			t.Error("cannot remove isolated test bind mount", err)
		}
	})
	if err := gate.check(t.Context()); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("changed mount topology passed the existing gate", err)
	}
	if current, err := os.ReadFile(original); err != nil || string(current) != string(data) {
		t.Fatal("source changed while testing topology", err)
	}
}

func TestPrivateArtifactStoreNeverWritesBindAliasedSources(t *testing.T) {
	host := os.Getenv("GALLERYGO_HOST_MOUNT_NAMESPACE")
	if host == "" {
		t.Skip("isolated user and mount namespace not requested")
	}
	current, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil || current == host || os.Geteuid() != 0 {
		t.Fatal("test requires a separate owned user and mount namespace", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal("cannot make the isolated namespace private", err)
	}
	for _, scenario := range []string{"separate", "source-as-cache", "source-under-cache", "cache-under-source", "source-child-as-cache", "cache-child-as-source", "unrelated-source-mount", "missing-source"} {
		t.Run(scenario, func(t *testing.T) {
			svc, artifact, data, original := privateArtifactFixture(t)
			source := filepath.Dir(original)
			var from, to string
			switch scenario {
			case "source-as-cache":
				from, to = source, svc.cacheRoot
			case "source-under-cache":
				from, to = source, filepath.Join(svc.cacheRoot, "variants")
				if err := os.Mkdir(to, 0o700); err != nil {
					t.Fatal(err)
				}
			case "cache-under-source":
				from, to = svc.cacheRoot, filepath.Join(source, "nested")
				if err := os.Mkdir(to, 0o700); err != nil {
					t.Fatal(err)
				}
			case "source-child-as-cache":
				from, to = filepath.Join(source, "album"), svc.cacheRoot
				if err := os.Mkdir(from, 0o700); err != nil {
					t.Fatal(err)
				}
			case "cache-child-as-source":
				from, to = filepath.Join(svc.cacheRoot, "subdir"), source
				if err := os.Mkdir(from, 0o700); err != nil {
					t.Fatal(err)
				}
			case "unrelated-source-mount":
				from, to = filepath.Join(filepath.Dir(source), "unrelated"), filepath.Join(source, "nested")
				for _, directory := range []string{from, to} {
					if err := os.Mkdir(directory, 0o700); err != nil {
						t.Fatal(err)
					}
				}
			case "missing-source":
				svc.sources["missing"] = SourceRoot{Path: filepath.Join(filepath.Dir(source), "missing"), Marker: ".marker"}
			}
			if from != "" {
				if err := unix.Mount(from, to, "", unix.MS_BIND, ""); err != nil {
					t.Fatal("cannot create an isolated synthetic bind mount", err)
				}
				t.Cleanup(func() {
					if err := unix.Unmount(to, unix.MNT_DETACH); err != nil {
						t.Error("cannot remove isolated test bind mount", err)
					}
				})
			}
			before, err := testSourceSnapshot(source)
			if err != nil {
				t.Fatal(err)
			}
			storeErr := svc.storeAndCloseArtifactForTest(t.Context(), "bind-test-job", artifact, data)
			if scenario == "separate" || scenario == "unrelated-source-mount" {
				if storeErr != nil {
					t.Error("separate private cache was rejected", storeErr)
				}
			} else if !errors.Is(storeErr, ErrVariantUnavailable) {
				t.Errorf("unverified or bind-aliased source was accepted: %v", storeErr)
			}
			if scenario == "missing-source" {
				entries, err := os.ReadDir(svc.cacheRoot)
				if err != nil || len(entries) != 0 {
					t.Error("unverified source caused cache writes", err)
				}
			}
			after, err := testSourceSnapshot(source)
			if err != nil {
				t.Fatal(err)
			}
			if len(before) != len(after) {
				t.Errorf("synthetic source tree changed: %d entries before, %d after", len(before), len(after))
			}
			for name, value := range before {
				if after[name] != value {
					t.Errorf("synthetic source entry changed: %q", name)
				}
			}
		})
	}
}
