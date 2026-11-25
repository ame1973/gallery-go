//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestPrivateArtifactStorageRefusesAliasedOrNoncanonicalInodes(t *testing.T) {
	for _, name := range []string{"source-hardlink", "replay-hardlink", "read-only", "setuid", "setgid", "sticky"} {
		t.Run(name, func(t *testing.T) {
			svc, artifact, data, original := privateArtifactFixture(t)
			target := filepath.Join(svc.cacheRoot, filepath.FromSlash(artifact.storage))
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "source-hardlink":
				if err := os.Link(original, target); err != nil {
					t.Fatal(err)
				}
			case "replay-hardlink":
				if err := svc.storeAndCloseArtifactForTest(t.Context(), "first-job", artifact, data); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(target, filepath.Join(filepath.Dir(svc.cacheRoot), "other-name.webp")); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(target, data, 0o600); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0o600)
				switch name {
				case "read-only":
					mode = 0o400
				case "setuid":
					mode |= os.ModeSetuid
				case "setgid":
					mode |= os.ModeSetgid
				case "sticky":
					mode |= os.ModeSticky
				}
				if err := os.Chmod(target, mode); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(target)
				if err != nil || info.Mode() != mode {
					t.Fatal("test filesystem did not preserve the requested mode", info, err)
				}
			}
			before, err := os.Lstat(target)
			if err != nil {
				t.Fatal(err)
			}
			if name == "source-hardlink" || name == "replay-hardlink" {
				stat, ok := before.Sys().(*syscall.Stat_t)
				if !ok || stat.Nlink != 2 {
					t.Fatal("test did not create a two-name inode")
				}
			}
			if err := svc.storeAndCloseArtifactForTest(t.Context(), "replay-job", artifact, data); !errors.Is(err, ErrVariantUnavailable) {
				t.Errorf("noncanonical existing inode was accepted: %v", err)
			}
			after, err := os.Lstat(target)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("refused target was replaced or changed", err)
			}
			if actual, err := os.ReadFile(target); err != nil || !bytes.Equal(actual, data) {
				t.Fatal("refused target bytes changed", err)
			}
			if actual, err := os.ReadFile(original); err != nil || !bytes.Equal(actual, data) {
				t.Fatal("read-only source changed", err)
			}
			requireNoArtifactTemps(t, svc.cacheRoot)
		})
	}
}

type artifactStatFileInfo struct {
	fs.FileInfo
	stat syscall.Stat_t
}

func (info artifactStatFileInfo) Sys() any { return &info.stat }

func TestPrivateArtifactPolicyRejectsDifferentOwnerDeviceOrLinkCount(t *testing.T) {
	svc, artifact, data, _ := privateArtifactFixture(t)
	target := filepath.Join(svc.cacheRoot, filepath.FromSlash(artifact.storage))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.Stat(svc.cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !artifactTempPrivate(info, cache, false) {
		t.Fatal("owned single-link fixture is not private")
	}
	for name, mutate := range map[string]func(*syscall.Stat_t){
		"different-owner":  func(stat *syscall.Stat_t) { stat.Uid ^= 1 },
		"different-device": func(stat *syscall.Stat_t) { stat.Dev ^= 1 },
		"extra-link":       func(stat *syscall.Stat_t) { stat.Nlink++ },
	} {
		t.Run(name, func(t *testing.T) {
			fake := artifactStatFileInfo{FileInfo: info, stat: *stat}
			mutate(&fake.stat)
			if artifactTempPrivate(fake, cache, false) {
				t.Fatal("nonprivate synthetic file identity was accepted")
			}
		})
	}
	root, err := os.OpenRoot(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := verifyArtifactFile(t.Context(), root, cache, filepath.Base(target), artifact); err != nil {
		t.Fatal("owned artifact failed the valid device control", err)
	}
	cacheStat, ok := cache.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("test filesystem does not expose a Linux stat")
	}
	foreignCache := artifactStatFileInfo{FileInfo: cache, stat: *cacheStat}
	foreignCache.stat.Dev ^= 1
	if err := verifyArtifactFile(t.Context(), root, foreignCache, filepath.Base(target), artifact); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("artifact on a different synthetic cache device was accepted", err)
	}
}

type artifactVerifyHookContext struct {
	context.Context
	calls int
	hook  func() error
	err   error
}

func (ctx *artifactVerifyHookContext) Err() error {
	ctx.calls++
	if ctx.calls == 2 {
		ctx.err = ctx.hook()
	}
	return ctx.Context.Err()
}

func TestPrivateArtifactVerificationRejectsRestoredMetadataAfterInodeChanges(t *testing.T) {
	for _, mutation := range []string{"temporary-hardlink", "restored-content"} {
		t.Run(mutation, func(t *testing.T) {
			svc, artifact, data, original := privateArtifactFixture(t)
			target := filepath.Join(svc.cacheRoot, filepath.FromSlash(artifact.storage))
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, data, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(filepath.Dir(target))
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			cacheInfo, err := os.Stat(svc.cacheRoot)
			if err != nil {
				t.Fatal(err)
			}
			ctx := &artifactVerifyHookContext{Context: t.Context()}
			ctx.hook = func() error {
				deadline := time.Now().Add(2 * time.Second)
				for {
					if mutation == "temporary-hardlink" {
						alias := filepath.Join(filepath.Dir(svc.cacheRoot), "transient-alias.webp")
						if err := os.Link(target, alias); err != nil {
							return err
						}
						if err := os.Remove(alias); err != nil {
							return err
						}
					} else {
						file, err := os.OpenFile(target, os.O_WRONLY, 0)
						if err != nil {
							return err
						}
						_, firstErr := file.WriteAt([]byte{data[0] ^ 1}, 0)
						_, secondErr := file.WriteAt(data[:1], 0)
						closeErr := file.Close()
						timeErr := os.Chtimes(target, before.ModTime(), before.ModTime())
						if err := errors.Join(firstErr, secondErr, closeErr, timeErr); err != nil {
							return err
						}
					}
					current, err := os.Stat(target)
					if err != nil {
						return err
					}
					if stillFileStamp(before) != stillFileStamp(current) {
						return nil
					}
					if time.Now().After(deadline) {
						return errors.New("test filesystem did not advance ctime")
					}
					time.Sleep(time.Millisecond)
				}
			}
			err = verifyArtifactFile(ctx, root, cacheInfo, filepath.Base(target), artifact)
			if ctx.calls < 2 || ctx.err != nil {
				t.Fatal("test mutation did not complete during verification", ctx.calls, ctx.err)
			}
			if !errors.Is(err, ErrVariantUnavailable) {
				t.Errorf("changed inode was accepted after restoring visible metadata: %v", err)
			}
			after, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || stillFileStamp(before) == stillFileStamp(after) {
				t.Fatalf("test mutation changed the wrong metadata: before=%v after=%v", before, after)
			}
			if actual, err := os.ReadFile(target); err != nil || !bytes.Equal(actual, data) {
				t.Fatal("test did not restore target bytes", err)
			}
			if actual, err := os.ReadFile(original); err != nil || !bytes.Equal(actual, data) {
				t.Fatal("test changed the source", err)
			}
		})
	}
}
