//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestContentDigestRejectsInFlightSameStatRewrite(t *testing.T) {
	name := filepath.Join(t.TempDir(), "sparse-source")
	created, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	const size = int64(512 << 20)
	if err := created.Truncate(size); err != nil {
		created.Close()
		t.Fatal(err)
	}
	created.Close()
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatal(err)
	}
	before, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	content := &Content{file: file, info: before, Size: size, ctx: t.Context()}
	done := make(chan error, 1)
	go func() { done <- content.verifyDigest(hex.EncodeToString(digest.Sum(nil))) }()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	fdinfo := filepath.Join("/proc/self/fdinfo", strconv.FormatUint(uint64(file.Fd()), 10))
	observed := false
	for !observed {
		data, err := os.ReadFile(fdinfo)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if value, ok := strings.CutPrefix(line, "pos:"); ok {
				position, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
				observed = err == nil && position > 0 && position < size
			}
		}
		if observed {
			break
		}
		select {
		case err := <-done:
			t.Fatal("digest finished before its I/O checkpoint", err)
		case <-deadline.C:
			t.Fatal("digest did not reach its I/O checkpoint")
		case <-tick.C:
		}
	}
	writer, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteAt([]byte{1}, size-1); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	writer.Close()
	if err := os.Chtimes(name, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(name)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !contentChangeTimeChanged(before, after) {
		t.Fatal("fixture did not preserve inode/size/mtime while changing ctime", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrContentChanged) {
			t.Fatal("same-stat rewrite survived a running digest", err)
		}
	case <-deadline.C:
		t.Fatal("rewritten content verification did not return")
	}
}

func TestContentHashReleasesWriterAndRechecksFreshAuthorization(t *testing.T) {
	for _, variant := range []bool{false, true} {
		name := "original"
		if variant {
			name = "variant"
		}
		t.Run(name, func(t *testing.T) {
			f := fixture(t)
			filename := f.original
			if variant {
				filename = f.variant
			}
			file, err := os.OpenFile(filename, os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				t.Fatal(err)
			}
			// Sparse fixture bytes prove I/O and authorization ordering, not media decoding.
			const size = int64(512 << 20)
			if err := file.Truncate(size); err != nil {
				file.Close()
				t.Fatal(err)
			}
			file.Close()
			read, err := os.Open(filename)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.New()
			if _, err := io.Copy(digest, read); err != nil {
				read.Close()
				t.Fatal(err)
			}
			read.Close()
			hash := hex.EncodeToString(digest.Sum(nil))
			info, err := os.Stat(filename)
			if err != nil {
				t.Fatal(err)
			}
			if variant {
				storage, err := VariantPath("revision", "fixture_png", 1, hash, "image/png")
				if err != nil {
					t.Fatal(err)
				}
				destination := filepath.Join(f.cache, filepath.FromSlash(storage))
				if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("fixture target already exists", err)
				}
				if err := os.Rename(filename, destination); err != nil {
					t.Fatal(err)
				}
				filename = destination
				if _, err := f.db.Exec(`UPDATE media_variants SET storage_path=?,size_bytes=?,sha256=? WHERE id='variant'`, storage, size, hash); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.db.Exec(`UPDATE source_entries SET size_bytes=?,mtime_ns=?,sha256=? WHERE id='entry'`, size, info.ModTime().UnixNano(), hash); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE media_revisions SET source_size=?,source_mtime_ns=?,source_sha256=? WHERE id='revision'`, size, info.ModTime().UnixNano(), hash); err != nil {
					t.Fatal(err)
				}
			}
			type outcome struct {
				content *Content
				err     error
			}
			done := make(chan outcome, 1)
			go func() {
				var content *Content
				var err error
				if variant {
					content, err = f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
				} else {
					content, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
				}
				done <- outcome{content, err}
			}()
			deadline := time.NewTimer(15 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			observed := false
			for !observed {
				entries, err := os.ReadDir("/proc/self/fd")
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
					if target != filename {
						continue
					}
					fdinfo, err := os.ReadFile(filepath.Join("/proc/self/fdinfo", entry.Name()))
					if err != nil {
						continue
					}
					for _, line := range strings.Split(string(fdinfo), "\n") {
						if value, ok := strings.CutPrefix(line, "pos:"); ok {
							position, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
							observed = err == nil && position > 0 && position < size
						}
					}
					if observed {
						break
					}
				}
				if observed {
					break
				}
				select {
				case result := <-done:
					if result.content != nil {
						result.content.Close()
					}
					t.Fatal("content hash finished before its I/O checkpoint", result.err)
				case <-deadline.C:
					t.Fatal("content descriptor was not opened")
				case <-tick.C:
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			tx, err := f.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal("streaming content hash held the SQLite writer", err)
			}
			if variant {
				_, err = tx.ExecContext(ctx, `DELETE FROM user_groups WHERE user_id=?`, f.reader.User.ID)
			} else {
				_, err = tx.ExecContext(ctx, `UPDATE users SET download_original=0 WHERE id=?`, f.reader.User.ID)
			}
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case result := <-done:
				if result.content != nil {
					result.content.Close()
				}
				if !errors.Is(result.err, sql.ErrNoRows) || len(f.svc.contentVerifies) != 0 {
					t.Fatal("revoked access survived the post-hash check or leaked a slot", result.err)
				}
			case <-deadline.C:
				t.Fatal("streamed verification did not return")
			}
		})
	}
}
