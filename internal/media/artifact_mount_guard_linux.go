//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

type artifactDirectoryIdentity struct {
	mountID, inode     uint64
	deviceMajor, minor uint32
}

type artifactSourceIdentity struct {
	path     string
	file     *os.File
	identity artifactDirectoryIdentity
}

func artifactMountData(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, ErrVariantUnavailable
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(inspectReader{ctx, file}, maxArtifactMountInfoBytes+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || len(data) == 0 || len(data) > maxArtifactMountInfoBytes {
		return nil, ErrVariantUnavailable
	}
	return data, nil
}

func artifactDirectoryIdentityForFD(fd int) (artifactDirectoryIdentity, error) {
	var stat unix.Statx_t
	required := uint32(unix.STATX_TYPE | unix.STATX_INO | unix.STATX_MNT_ID)
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, int(required), &stat); err != nil || stat.Mask&required != required || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mnt_id == 0 {
		return artifactDirectoryIdentity{}, ErrVariantUnavailable
	}
	return artifactDirectoryIdentity{stat.Mnt_id, stat.Ino, stat.Dev_major, stat.Dev_minor}, nil
}

func openArtifactSourceDirectory(path string) (*os.File, artifactDirectoryIdentity, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return nil, artifactDirectoryIdentity{}, ErrVariantUnavailable
	}
	how := unix.OpenHow{
		Flags:   uint64(unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NONBLOCK),
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &how)
	if err != nil {
		return nil, artifactDirectoryIdentity{}, ErrVariantUnavailable
	}
	file := os.NewFile(uintptr(fd), "private source directory")
	identity, err := artifactDirectoryIdentityForFD(fd)
	if err != nil {
		file.Close()
		return nil, artifactDirectoryIdentity{}, err
	}
	return file, identity, nil
}

func artifactSourceDirectory(path string) (artifactDirectoryIdentity, error) {
	file, identity, err := openArtifactSourceDirectory(path)
	if err != nil {
		return artifactDirectoryIdentity{}, err
	}
	if err := file.Close(); err != nil {
		return artifactDirectoryIdentity{}, ErrVariantUnavailable
	}
	return identity, nil
}

func artifactCacheDirectory(root *os.Root) (artifactDirectoryIdentity, error) {
	file, err := root.Open(".")
	if err != nil {
		return artifactDirectoryIdentity{}, ErrVariantUnavailable
	}
	defer file.Close()
	return artifactDirectoryIdentityForFD(int(file.Fd()))
}

func artifactObservedMount(mounts map[uint64]artifactMount, path string, identity artifactDirectoryIdentity) error {
	mount, _, err := artifactMountLocation(mounts, identity.mountID, path)
	if err != nil || mount.device != fmt.Sprintf("%d:%d", identity.deviceMajor, identity.minor) {
		return ErrVariantUnavailable
	}
	return nil
}

// The guard detects aliases represented in the current mount namespace. Unknown
// backing/network aliases and privileged mount changes remain operator boundaries.
func (s *Service) artifactMountGuard(ctx context.Context, root *os.Root) (func(context.Context) error, []*os.File, error) {
	data, err := artifactMountData(ctx)
	if err != nil {
		return nil, nil, err
	}
	mounts, err := readArtifactMounts(bytes.NewReader(data))
	if err != nil {
		return nil, nil, ErrVariantUnavailable
	}
	cache, err := artifactCacheDirectory(root)
	if err != nil || artifactObservedMount(mounts, s.cacheRoot, cache) != nil {
		return nil, nil, ErrVariantUnavailable
	}
	paths := make([]string, 0, len(s.sources))
	for _, source := range s.sources {
		paths = append(paths, source.Path)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	sources := make([]artifactSourceIdentity, 0, len(paths))
	keep := false
	defer func() {
		if !keep {
			for _, source := range sources {
				source.file.Close()
			}
		}
	}()
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		file, identity, err := openArtifactSourceDirectory(path)
		if err != nil {
			return nil, nil, ErrVariantUnavailable
		}
		sources = append(sources, artifactSourceIdentity{path: path, file: file, identity: identity})
		if artifactObservedMount(mounts, path, identity) != nil {
			return nil, nil, ErrVariantUnavailable
		}
		overlaps, err := artifactMountSourceOverlap(mounts, cache.mountID, s.cacheRoot, identity.mountID, path)
		if err != nil || overlaps {
			return nil, nil, ErrVariantUnavailable
		}
	}
	fingerprint := sha256.Sum256(data)
	check := func(ctx context.Context) error {
		data, err := artifactMountData(ctx)
		if err != nil {
			return err
		}
		if sha256.Sum256(data) != fingerprint {
			return ErrVariantUnavailable
		}
		current, err := artifactCacheDirectory(root)
		if err != nil || current != cache {
			return ErrVariantUnavailable
		}
		for _, source := range sources {
			if err := ctx.Err(); err != nil {
				return err
			}
			pinned, err := artifactDirectoryIdentityForFD(int(source.file.Fd()))
			if err != nil || pinned != source.identity {
				return ErrVariantUnavailable
			}
			current, err := artifactSourceDirectory(source.path)
			if err != nil || current != source.identity {
				return ErrVariantUnavailable
			}
		}
		data, err = artifactMountData(ctx)
		if err != nil {
			return err
		}
		if sha256.Sum256(data) != fingerprint {
			return ErrVariantUnavailable
		}
		return ctx.Err()
	}
	if err := check(ctx); err != nil {
		return nil, nil, err
	}
	files := make([]*os.File, 0, len(sources))
	for _, source := range sources {
		files = append(files, source.file)
	}
	keep = true
	return check, files, nil
}
