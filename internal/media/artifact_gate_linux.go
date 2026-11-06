//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const artifactGateName = ".gallerygo-artifacts.lock"

func localArtifactFilesystem(kind int64) bool {
	switch kind {
	case unix.EXT4_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.TMPFS_MAGIC, unix.OVERLAYFS_SUPER_MAGIC:
		return true
	default:
		return false
	}
}

func (s *Service) openArtifactCache() (*os.Root, fs.FileInfo, error) {
	if !filepath.IsAbs(s.cacheRoot) || filepath.Clean(s.cacheRoot) != s.cacheRoot {
		return nil, nil, ErrVariantUnavailable
	}
	for _, source := range s.sources {
		if !filepath.IsAbs(source.Path) || artifactPathsOverlap(s.cacheRoot, source.Path) {
			return nil, nil, ErrVariantUnavailable
		}
	}
	resolved, err := filepath.EvalSymlinks(s.cacheRoot)
	if err != nil || resolved != s.cacheRoot {
		return nil, nil, ErrVariantUnavailable
	}
	listed, err := os.Lstat(s.cacheRoot)
	if err != nil || !listed.IsDir() || listed.Mode().Perm()&0o022 != 0 {
		return nil, nil, ErrVariantUnavailable
	}
	root, err := os.OpenRoot(s.cacheRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(listed, opened) {
		root.Close()
		return nil, nil, ErrVariantUnavailable
	}
	return root, listed, nil
}

func privateArtifactGateFile(info fs.FileInfo) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1 && stat.Uid == uint32(os.Geteuid()) && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && info.Size() == 0 && info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
}

// lockArtifacts does no DB work. The caller must never hold a SQLite writer while
// opening/waiting on filesystem locks. Each call opens an independent description;
// there is no lock conversion, time-based unlock or descriptor grant to children.
func (s *Service) lockArtifacts(ctx context.Context, exclusive bool) (*artifactGate, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, info, err := s.openArtifactCache()
	if err != nil {
		return nil, err
	}
	gate := &artifactGate{root: root, rootInfo: info, cacheRoot: s.cacheRoot, exclusive: exclusive}
	fail := func(err error) (*artifactGate, error) { gate.Close(); return nil, err }
	directory, err := root.Open(".")
	if err != nil {
		return fail(err)
	}
	defer directory.Close()
	var filesystem unix.Statfs_t
	if err := unix.Fstatfs(int(directory.Fd()), &filesystem); err != nil || !localArtifactFilesystem(filesystem.Type) {
		return fail(ErrVariantUnavailable)
	}
	file, err := root.OpenFile(artifactGateName, os.O_CREATE|os.O_EXCL|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		file, err = root.OpenFile(artifactGateName, os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	}
	if err != nil {
		return fail(fmt.Errorf("%w: %w", ErrVariantUnavailable, err))
	}
	gate.file = file
	gate.lockInfo, err = file.Stat()
	if err != nil || !privateArtifactGateFile(gate.lockInfo) {
		return fail(ErrVariantUnavailable)
	}
	if created {
		if err := file.Sync(); err != nil {
			return fail(err)
		}
		if err := directory.Sync(); err != nil {
			return fail(err)
		}
	}
	operation := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		operation = unix.LOCK_EX | unix.LOCK_NB
	}
	// ponytail: one cache-wide gate; shard only after measured GC contention.
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := gate.check(ctx); err != nil {
			return fail(err)
		}
		err := unix.Flock(int(file.Fd()), operation)
		if err == nil {
			if err := gate.check(ctx); err != nil {
				return fail(err)
			}
			return gate, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return fail(fmt.Errorf("%w: %w", ErrVariantUnavailable, err))
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-tick.C:
		}
	}
}

// check performs filesystem IO, never a publication transaction's DB-only checks.
// Advisory exclusion assumes the private cache is not modified by unrelated code.
func (gate *artifactGate) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if gate == nil || gate.file == nil || gate.root == nil {
		return ErrVariantUnavailable
	}
	listed, err := os.Lstat(gate.cacheRoot)
	if err != nil || !listed.IsDir() || listed.Mode().Perm()&0o022 != 0 || !os.SameFile(listed, gate.rootInfo) {
		return ErrVariantUnavailable
	}
	opened, err := gate.root.Stat(".")
	if err != nil || !os.SameFile(opened, gate.rootInfo) {
		return ErrVariantUnavailable
	}
	named, err := gate.root.Lstat(artifactGateName)
	if err != nil || !privateArtifactGateFile(named) || !os.SameFile(named, gate.lockInfo) {
		return ErrVariantUnavailable
	}
	current, err := gate.file.Stat()
	if err != nil || !privateArtifactGateFile(current) || !os.SameFile(current, gate.lockInfo) {
		return ErrVariantUnavailable
	}
	return ctx.Err()
}
