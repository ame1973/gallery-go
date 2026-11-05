//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func privateArtifactFixture(t *testing.T) (*Service, stillArtifact, []byte, string) {
	t.Helper()
	base := t.TempDir()
	cache, source := filepath.Join(base, "cache"), filepath.Join(base, "source")
	for _, name := range []string{cache, source} {
		if err := os.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	data := webpFixture(t, "lossless")
	original := filepath.Join(source, "owned.webp")
	if err := os.WriteFile(original, data, 0o400); err != nil {
		t.Fatal(err)
	}
	artifact, err := validateStillArtifact(t.Context(), "revision-private", "thumb_320", stillSource(3, 2, nil), data)
	if err != nil {
		t.Fatal(err)
	}
	return New(nil, nil, cache, map[string]SourceRoot{"owned": {Path: source, Marker: ".marker"}}), artifact, data, original
}

func requireNoArtifactTemps(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Error("private temporary output was not cleaned", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateArtifactStorageIsImmutableAtomicAndReplaySafe(t *testing.T) {
	svc, artifact, data, original := privateArtifactFixture(t)
	if err := svc.storeStillArtifact(t.Context(), "job-one", artifact, data); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(svc.cacheRoot, filepath.FromSlash(artifact.storage))
	before, err := os.Stat(name)
	if err != nil || before.Mode().Perm() != 0o600 || !before.Mode().IsRegular() {
		t.Fatal("artifact permissions/type are not private", before, err)
	}
	for _, job := range []string{"job-one", "job-two"} {
		if err := svc.storeStillArtifact(t.Context(), job, artifact, data); err != nil {
			t.Fatal("same bytes/key must reuse without overwrite", job, err)
		}
	}
	after, err := os.Stat(name)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("immutable replay replaced the existing inode", before, after, err)
	}
	if actual, err := os.ReadFile(name); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("stored bytes differ", err)
	}
	for directory := filepath.Dir(name); directory != svc.cacheRoot; directory = filepath.Dir(directory) {
		info, err := os.Stat(directory)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatal("artifact parent is not private", directory, info, err)
		}
	}
	if actual, err := os.ReadFile(original); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("source bytes changed", err)
	}
	info, err := os.Stat(original)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatal("source mode changed", info, err)
	}
	requireNoArtifactTemps(t, svc.cacheRoot)
}

func TestConcurrentPrivateArtifactWritersNeverReplaceOrExposePartialFiles(t *testing.T) {
	svc, artifact, data, _ := privateArtifactFixture(t)
	var group sync.WaitGroup
	failures := make(chan error, 12)
	for range 12 {
		group.Go(func() { failures <- svc.storeStillArtifact(t.Context(), "same-live-job", artifact, data) })
	}
	finished := make(chan struct{})
	go func() {
		group.Wait()
		close(finished)
	}()
	name := filepath.Join(svc.cacheRoot, filepath.FromSlash(artifact.storage))
	var observationError error
observe:
	for {
		actual, err := os.ReadFile(name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			observationError = err
		}
		if err == nil && !bytes.Equal(actual, data) {
			observationError = errors.New("partial artifact observed at immutable key")
		}
		select {
		case <-finished:
			break observe
		default:
			runtime.Gosched()
		}
	}
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal("concurrent immutable storage failed", err)
		}
	}
	if observationError != nil {
		t.Fatal("a concurrent reader observed partial output", observationError)
	}
	if actual, err := os.ReadFile(name); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("concurrent output mixed bytes", err)
	}
	entries, err := os.ReadDir(filepath.Dir(name))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(name) {
		t.Fatal("duplicate or partial immutable output", entries, err)
	}
	requireNoArtifactTemps(t, svc.cacheRoot)
}

func TestPrivateArtifactStorageRefusesCorruptExistingFilesAndMutableClaims(t *testing.T) {
	svc, artifact, data, _ := privateArtifactFixture(t)
	name := filepath.Join(svc.cacheRoot, filepath.FromSlash(artifact.storage))
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	poison := bytes.Repeat([]byte{0x5a}, len(data))
	if err := os.WriteFile(name, poison, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(name)
	if err := svc.storeStillArtifact(t.Context(), "job-corrupt", artifact, data); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("corrupt immutable target was overwritten or blessed", err)
	}
	after, _ := os.Stat(name)
	if actual, err := os.ReadFile(name); err != nil || !bytes.Equal(actual, poison) || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("conflicting target was modified", err)
	}
	for _, change := range []func(*stillArtifact){
		func(a *stillArtifact) { a.storage = "../source/owned.webp" },
		func(a *stillArtifact) { a.hash = strings.Repeat("0", 64) },
		func(a *stillArtifact) { a.presetVersion++ },
		func(a *stillArtifact) { a.width++ },
		func(a *stillArtifact) { a.size++ },
		func(a *stillArtifact) { a.mime = "image/png" },
	} {
		forged := artifact
		change(&forged)
		if err := svc.storeStillArtifact(t.Context(), "job-forged", forged, data); !errors.Is(err, errStillArtifact) {
			t.Fatal("mutable artifact claim bypassed byte/key validation", forged, err)
		}
	}
	if err := svc.storeStillArtifact(t.Context(), "job-new-bytes", artifact, webpFixture(t, "lossy")); !errors.Is(err, errStillArtifact) {
		t.Fatal("data changed after validation was accepted", err)
	}
	requireNoArtifactTemps(t, svc.cacheRoot)
}

func TestPrivateArtifactStorageRejectsSourceOverlapAliasesAndUnsafeJobs(t *testing.T) {
	svc, artifact, data, original := privateArtifactFixture(t)
	for _, job := range []string{"", ".", "../escape", "/absolute", "one/two", `one\two`, "job\x00", "job\n", strings.Repeat("j", 129)} {
		if err := svc.storeStillArtifact(t.Context(), job, artifact, data); !errors.Is(err, errStillArtifact) {
			t.Fatal("unsafe job folder accepted", job, err)
		}
	}
	for _, cache := range []string{"relative", "", filepath.Join(svc.cacheRoot, "missing"), filepath.Dir(original), filepath.Dir(filepath.Dir(original))} {
		bad := New(nil, nil, cache, svc.sources)
		if err := bad.storeStillArtifact(t.Context(), "job", artifact, data); !errors.Is(err, ErrVariantUnavailable) {
			t.Fatal("unconfigured/missing/overlapping private root accepted", cache, err)
		}
	}
	alias := filepath.Join(filepath.Dir(svc.cacheRoot), "cache-alias")
	if err := os.Symlink(svc.cacheRoot, alias); err != nil {
		t.Fatal(err)
	}
	if err := New(nil, nil, alias, svc.sources).storeStillArtifact(t.Context(), "job", artifact, data); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("cache-root alias introduced after config validation accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := svc.storeStillArtifact(ctx, "job", artifact, data); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled storage created output", err)
	}
	if entries, err := os.ReadDir(svc.cacheRoot); err != nil || len(entries) != 0 {
		t.Fatal("rejected request created cache contents", entries, err)
	}
	if actual, err := os.ReadFile(original); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("root rejection touched source", err)
	}
}

func TestPrivateArtifactDirectoriesAndTargetsNeverFollowSourceSymlinks(t *testing.T) {
	for _, location := range []string{"tmp", "variants", "target", "internal-alias"} {
		t.Run(location, func(t *testing.T) {
			svc, artifact, data, original := privateArtifactFixture(t)
			link := filepath.Join(svc.cacheRoot, location)
			target := filepath.Dir(original)
			if location == "target" {
				link, target = filepath.Join(svc.cacheRoot, filepath.FromSlash(artifact.storage)), original
				if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
					t.Fatal(err)
				}
			} else if location == "internal-alias" {
				if err := os.Mkdir(filepath.Join(svc.cacheRoot, "another"), 0o700); err != nil {
					t.Fatal(err)
				}
				link, target = filepath.Join(svc.cacheRoot, "variants"), "another"
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if err := svc.storeStillArtifact(t.Context(), "job-symlink", artifact, data); !errors.Is(err, ErrVariantUnavailable) {
				t.Fatal("artifact storage followed an unowned link", err)
			}
			if actual, err := os.ReadFile(original); err != nil || !bytes.Equal(actual, data) {
				t.Fatal("artifact symlink mutated source", err)
			}
			if entries, err := os.ReadDir(filepath.Dir(original)); err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(original) {
				t.Fatal("artifact directories were created in source", entries, err)
			}
		})
	}
}

func TestPrivateArtifactStorageRejectsUntrustedWritableDirectories(t *testing.T) {
	for _, location := range []string{"root", "tmp", "variants"} {
		t.Run(location, func(t *testing.T) {
			svc, artifact, data, original := privateArtifactFixture(t)
			directory := svc.cacheRoot
			if location != "root" {
				directory = filepath.Join(directory, location)
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(directory, 0o777); err != nil {
				t.Fatal(err)
			}
			if err := svc.storeStillArtifact(t.Context(), "job-writable", artifact, data); !errors.Is(err, ErrVariantUnavailable) {
				t.Fatal("group/world-writable storage directory accepted", location, err)
			}
			if actual, err := os.ReadFile(original); err != nil || !bytes.Equal(actual, data) {
				t.Fatal("directory rejection touched source", err)
			}
		})
	}
}

func TestPrivateArtifactBytesNeverCreateReadyRowsOrDownloadAuthority(t *testing.T) {
	f := fixture(t)
	if err := os.Chmod(f.original, 0o400); err != nil {
		t.Fatal(err)
	}
	counts := func() [5]int64 {
		t.Helper()
		var result [5]int64
		if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media_revisions),(SELECT COUNT(*) FROM media_variants),(SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`).Scan(&result[0], &result[1], &result[2], &result[3], &result[4]); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := counts()
	data := webpFixture(t, "lossless")
	artifact, err := validateStillArtifact(t.Context(), "unpublished-revision", "thumb_320", stillSource(3, 2, nil), data)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.storeStillArtifact(t.Context(), "unpublished-job", artifact, data); err != nil {
		t.Fatal(err)
	}
	if counts() != before {
		t.Fatal("filesystem staging mutated business/READY/events/audit rows")
	}
	variants, err := f.svc.Variants(t.Context(), f.reader, f.mediaID, "", 30)
	if err != nil || len(variants) != 1 || variants[0].ID != "variant" {
		t.Fatal("unpublished artifact was advertised", variants, err)
	}
	if content, err := f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "unpublished-job"); !errors.Is(err, sql.ErrNoRows) || content != nil {
		t.Fatal("private bytes became a download capability", content, err)
	}
	if actual, err := os.ReadFile(f.original); err != nil || !bytes.Equal(actual, f.data) {
		t.Fatal("staging changed source bytes", err)
	}
	requireNoArtifactTemps(t, f.cache)
}
