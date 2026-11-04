package scanner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/media"
)

func walkRoot(t *testing.T) media.SourceRoot {
	t.Helper()
	root := media.SourceRoot{Path: t.TempDir(), Marker: ".mounted"}
	if err := os.WriteFile(filepath.Join(root.Path, root.Marker), []byte("isolated read-only mount"), 0o400); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeInput(t *testing.T, root media.SourceRoot, name string, data []byte) string {
	t.Helper()
	full := filepath.Join(root.Path, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, 0o400); err != nil {
		t.Fatal(err)
	}
	return full
}

func walkError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v want=%v", err, want)
	}
}

func TestWalkStreamsBoundedManifestAndPreservesDistinctPaths(t *testing.T) {
	root := walkRoot(t)
	data := bytes.Repeat([]byte("source bytes"), 20000)
	original := writeInput(t, root, "Person/Album/original.bin", data)
	writeInput(t, root, "人物/相簿/empty.bin", nil)
	if err := os.Link(original, filepath.Join(root.Path, "hard-link.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("Person/Album/original.bin", filepath.Join(root.Path, "confined-link.bin")); err != nil {
		t.Fatal(err)
	}
	var previous map[string]File
	for _, batch := range []int{1, 128} {
		policy := DefaultPolicy()
		policy.BatchSize = batch
		files := map[string]File{}
		summary, err := Walk(t.Context(), root, policy, func(file File) error {
			if _, exists := files[file.RelativePath]; exists {
				t.Fatal("path enumerated twice")
			}
			files[file.RelativePath] = file
			return nil
		})
		if err != nil || summary.Files != 4 || summary.Bytes != int64(3*len(data)) || summary.Directories != 5 {
			t.Fatal(summary, err)
		}
		if _, ok := files[root.Marker]; ok {
			t.Fatal("mount marker became indexed content")
		}
		hash := sha256.Sum256(data)
		if files["Person/Album/original.bin"].SHA256 != hex.EncodeToString(hash[:]) || files["hard-link.bin"].SHA256 != files["confined-link.bin"].SHA256 {
			t.Fatal("streamed hash or alias identity changed")
		}
		quick := sha256.New()
		quick.Write(data[:hashChunk])
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(data)))
		quick.Write(size[:])
		if files["Person/Album/original.bin"].QuickFingerprint != hex.EncodeToString(quick.Sum(nil)) {
			t.Fatal("quick stat-bound fingerprint mismatch")
		}
		if previous != nil && !reflect.DeepEqual(previous, files) {
			t.Fatal("bounded enumeration changed inventory semantics")
		}
		previous = files
	}
	actual, err := os.ReadFile(original)
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("walk modified source bytes", err)
	}
	if previous["hard-link.bin"].Identity != previous["Person/Album/original.bin"].Identity {
		t.Fatal("hard link stat identity was not preserved")
	}
}

func TestWalkRejectsCyclesEscapesAndNonRegularFiles(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		root := walkRoot(t)
		writeInput(t, root, "folder/file", []byte("safe"))
		if err := os.Symlink("..", filepath.Join(root.Path, "folder", "loop")); err != nil {
			t.Fatal(err)
		}
		_, err := Walk(t.Context(), root, DefaultPolicy(), func(File) error { return nil })
		walkError(t, err, ErrCycle)
	})
	t.Run("escape", func(t *testing.T) {
		root := walkRoot(t)
		outside := filepath.Join(t.TempDir(), "private")
		if err := os.WriteFile(outside, []byte("outside"), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root.Path, "escape")); err != nil {
			t.Fatal(err)
		}
		_, err := Walk(t.Context(), root, DefaultPolicy(), func(file File) error {
			if file.RelativePath == "escape" {
				t.Fatal("escaping link bytes were inventoried")
			}
			return nil
		})
		if err == nil {
			t.Fatal("escaping link accepted")
		}
	})
	t.Run("fifo", func(t *testing.T) {
		root := walkRoot(t)
		fifo := filepath.Join(root.Path, "pipe")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := Walk(t.Context(), root, DefaultPolicy(), func(File) error { return nil })
			result <- err
		}()
		select {
		case err := <-result:
			walkError(t, err, ErrUnsafeEntry)
		case <-time.After(2 * time.Second):
			if writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				writer.Close()
			}
			t.Fatal("scanner blocked on FIFO")
		}
	})
}

func TestWalkResourceLimitsAndUnsafeNames(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy func(*Policy)
		files  []string
		want   error
	}{
		{"files", func(p *Policy) { p.MaxFiles = 1 }, []string{"one", "two"}, ErrLimit},
		{"bytes", func(p *Policy) { p.MaxFileBytes = 1 }, []string{"one"}, ErrLimit},
		{"depth", func(p *Policy) { p.MaxDepth = 1 }, []string{"one/two/file"}, ErrLimit},
		{"backslash", func(*Policy) {}, []string{"bad\\path"}, ErrUnsafeEntry},
		{"invalid UTF-8", func(*Policy) {}, []string{string([]byte{'b', 0xff})}, ErrUnsafeEntry},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := walkRoot(t)
			for _, name := range test.files {
				writeInput(t, root, name, []byte("bytes"))
			}
			policy := DefaultPolicy()
			test.policy(&policy)
			_, err := Walk(t.Context(), root, policy, func(File) error { return nil })
			walkError(t, err, test.want)
		})
	}
	for _, policy := range []Policy{{}, func() Policy { p := DefaultPolicy(); p.BatchSize = 257; return p }(), func() Policy { p := DefaultPolicy(); p.Timeout = 25 * time.Hour; return p }()} {
		walkError(t, policy.Validate(), ErrPolicy)
	}
	if validRelative("../outside") || validRelative("/absolute") || validRelative(strings.Repeat("x", maxRelativePath+1)) {
		t.Fatal("invalid normalized source path accepted")
	}
}

type readContext struct {
	context.Context
	calls     int
	afterRead func()
}

func (ctx *readContext) Err() error {
	ctx.calls++
	if ctx.calls == 2 {
		ctx.afterRead()
	}
	return ctx.Context.Err()
}

func TestHashRejectsInPlaceChangeOrPathReplacementDuringRead(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "overwrite", true: "replace"}[replace], func(t *testing.T) {
			root := walkRoot(t)
			data := bytes.Repeat([]byte("a"), hashChunk*4)
			name := writeInput(t, root, "changing.bin", data)
			listed, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			ctx := &readContext{Context: t.Context(), afterRead: func() {
				if replace {
					replacement := filepath.Join(root.Path, "replacement")
					if err := os.WriteFile(replacement, data, 0o400); err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(replacement, listed.ModTime(), listed.ModTime()); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(replacement, name); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Chmod(name, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(name, bytes.Repeat([]byte("b"), len(data)), 0o400); err != nil {
						t.Fatal(err)
					}
					stamp := listed.ModTime().Add(time.Second)
					if err := os.Chtimes(name, stamp, stamp); err != nil {
						t.Fatal(err)
					}
				}
			}}
			confined, err := os.OpenRoot(root.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer confined.Close()
			walker := walk{ctx: ctx, root: confined, policy: DefaultPolicy(), buffer: make([]byte, hashChunk)}
			_, err = walker.hashFile("changing.bin", listed)
			walkError(t, err, ErrChanged)
		})
	}
}

func TestDirectoryStampDetectsChangedNamesEvenWhenStatMatches(t *testing.T) {
	root := walkRoot(t)
	writeInput(t, root, "one", []byte("one"))
	entries, err := os.ReadDir(root.Path)
	if err != nil {
		t.Fatal(err)
	}
	var expected directoryStamp
	for _, entry := range entries {
		expected.add(entry)
	}
	writeInput(t, root, "added", []byte("new"))
	// Feed the current stat as the baseline to model coarse/equal metadata. Only
	// the independently captured names/types distinguish the stale enumeration.
	info, err := os.Stat(root.Path)
	if err != nil {
		t.Fatal(err)
	}
	confined, err := os.OpenRoot(root.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer confined.Close()
	walker := walk{ctx: t.Context(), root: confined, policy: DefaultPolicy()}
	walkError(t, walker.verifyDirectory(".", info, expected), ErrChanged)
}

func TestWalkCancellationCallbackFailureAndChangedDirectory(t *testing.T) {
	root := walkRoot(t)
	writeInput(t, root, "one", []byte("one"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Walk(ctx, root, DefaultPolicy(), func(File) error { return nil })
	walkError(t, err, context.Canceled)
	failure := errors.New("staging failed")
	_, err = Walk(t.Context(), root, DefaultPolicy(), func(File) error { return failure })
	walkError(t, err, failure)
	changed := false
	seen := map[string]bool{}
	_, err = Walk(t.Context(), root, DefaultPolicy(), func(file File) error {
		seen[file.RelativePath] = true
		if !changed {
			changed = true
			writeInput(t, root, "added-during-walk", []byte("new"))
		}
		return nil
	})
	if err != nil {
		walkError(t, err, ErrChanged)
	} else if !seen["one"] || !seen["added-during-walk"] || len(seen) != 2 {
		t.Fatal("changed directory silently published an incomplete manifest", seen)
	}
	_, err = Walk(t.Context(), root, DefaultPolicy(), nil)
	walkError(t, err, ErrPolicy)
	if err := os.Remove(filepath.Join(root.Path, root.Marker)); err != nil {
		t.Fatal(err)
	}
	_, err = Walk(t.Context(), root, DefaultPolicy(), func(File) error { return nil })
	if err == nil {
		t.Fatal("missing mount marker accepted")
	}
}
