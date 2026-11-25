//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArtifactSafeOpenRejectsAmbiguousNamesAndSymlinks(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(directory, "file"), []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"file-link": "file", "dir-link": "child"} {
		if err := os.Symlink(target, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"", ".", "..", "../file", "/file", "child/file", `child\file`, "file\x00", "file-link"} {
		file, err := openArtifactAt(root, name, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if file != nil {
			file.Close()
		}
		if err == nil {
			t.Errorf("unsafe file name accepted: %q", name)
		}
	}
	if child, _, err := openArtifactDirectory(root, "dir-link"); err == nil {
		child.Close()
		t.Fatal("symbolic-link directory accepted")
	}
	file, err := openArtifactAt(root, "file", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 5)
	if n, err := file.Read(data); err != nil || n != 5 || string(data) != "owned" {
		file.Close()
		t.Fatal("ordinary private file was not readable", n, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	child, info, err := openArtifactDirectory(root, "child")
	if err != nil || !info.IsDir() {
		t.Fatal("ordinary private directory was not opened", err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
}

// Bind mounts are created only in an explicitly supplied private user/mount
// namespace. Ordinary test runs cannot accidentally alter the host namespace.
func TestArtifactDirectoryNeverCrossesNestedMount(t *testing.T) {
	host := os.Getenv("GALLERYGO_TEST_PARENT_MOUNT_NAMESPACE")
	if host == "" {
		t.Skip("isolated user and mount namespace not requested")
	}
	current, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil || current == host || os.Geteuid() != 0 {
		t.Fatal("test requires a separate owned user and mount namespace", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal("cannot make test namespace private", err)
	}
	for _, scenario := range []string{"storage-walk", "reconcile-walk", "gate-file", "artifact-file", "pinned-directory"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			cache, source := filepath.Join(base, "cache"), filepath.Join(base, "source")
			for _, name := range []string{cache, source} {
				if err := os.Mkdir(name, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(source, "canary"), []byte("owned source"), 0o400); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(cache)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			bind := func(from, to string) {
				t.Helper()
				if err := unix.Mount(from, to, "", unix.MS_BIND, ""); err != nil {
					t.Fatal("cannot create isolated test bind mount", err)
				}
				t.Cleanup(func() {
					if err := unix.Unmount(to, unix.MNT_DETACH); err != nil {
						t.Error("cannot remove isolated test bind mount", err)
					}
				})
			}
			if scenario == "gate-file" {
				from, to := filepath.Join(source, "foreign-lock"), filepath.Join(cache, artifactGateName)
				for _, name := range []string{from, to} {
					if err := os.WriteFile(name, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				bind(from, to)
				svc := New(nil, nil, cache, map[string]SourceRoot{"owned": {Path: source, Marker: ".marker"}})
				gate, err := svc.lockArtifacts(t.Context(), false)
				if gate != nil {
					gate.Close()
				}
				if err == nil {
					t.Error("gate opened a file mounted from the source")
				}
			} else if scenario == "artifact-file" {
				data := webpFixture(t, "lossless")
				artifact, err := validateStillArtifact(t.Context(), "revision", "thumb_320", stillSource(3, 2, nil), data)
				if err != nil {
					t.Fatal(err)
				}
				from, to := filepath.Join(source, "foreign-artifact"), filepath.Join(cache, "target.webp")
				if err := os.WriteFile(from, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(to, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				bind(from, to)
				info, err := root.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
				if err := verifyArtifactFile(t.Context(), root, info, "target.webp", artifact); err == nil {
					t.Error("artifact verification opened a file mounted from the source")
				}
			} else if scenario == "pinned-directory" {
				pinned, err := makeArtifactDirectory(root, "variants/staged")
				if err != nil {
					t.Fatal(err)
				}
				defer pinned.Close()
				bind(source, filepath.Join(cache, "variants", "staged"))
				file, err := pinned.OpenFile("owned-output", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.Write([]byte("private")); err != nil {
					file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				mountpoint := filepath.Join(cache, "variants")
				if err := os.Mkdir(mountpoint, 0o700); err != nil {
					t.Fatal(err)
				}
				bind(source, mountpoint)
				if scenario == "storage-walk" {
					opened, err := makeArtifactDirectory(root, "variants/revision")
					if opened != nil {
						opened.Close()
					}
					if err == nil {
						t.Error("storage traversed a source bind mount")
					}
				} else {
					info, err := root.Stat(".")
					if err != nil {
						t.Fatal(err)
					}
					opened, _, err := openArtifactTempDirectory(root, "variants", info)
					if opened != nil {
						opened.Close()
					}
					if err == nil {
						t.Error("reconciliation traversed a source bind mount")
					}
				}
			}
			entries, err := os.ReadDir(source)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if scenario == "gate-file" || scenario == "artifact-file" {
				want = 2
			}
			if len(entries) != want {
				t.Errorf("source directory was changed: got %d entries, want %d", len(entries), want)
			}
			if data, err := os.ReadFile(filepath.Join(source, "canary")); err != nil || string(data) != "owned source" {
				t.Fatal("source canary changed", err)
			}
			if scenario == "artifact-file" {
				data, err := os.ReadFile(filepath.Join(source, "foreign-artifact"))
				if err != nil || !bytes.Equal(data, webpFixture(t, "lossless")) {
					t.Fatal("mounted source artifact changed", err)
				}
			}
		})
	}
}
