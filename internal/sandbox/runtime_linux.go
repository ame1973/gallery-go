//go:build linux && (amd64 || arm64)

package sandbox

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// Signature identifies the fixed installed runtime outside database transactions.
// Trusted system files are pinned by canonical path, inode, size, mode and both
// timestamps; unlike source media, they are not attacker-writable NAS inputs.
func Signature(ctx context.Context) (string, error) { return signatureFor(ctx, profileProbe) }
func ImageSignature(ctx context.Context) (string, error) {
	return signatureFor(ctx, profileImageHeader)
}

func signatureFor(ctx context.Context, profile codecProfile) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	files, err := runtimeFilesFor(profile)
	if err == nil {
		var pin string
		pin, err = runtimeFingerprint(ctx, files, nil)
		if err == nil {
			return pin, nil
		}
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return "", ErrUnavailable
}

// The child's fingerprint uses the very O_PATH descriptors granted to Landlock,
// not a later pathname lookup that could bless a different installation epoch.
func runtimeFingerprint(ctx context.Context, files []string, grant func(string, int) error) (string, error) {
	if len(files) < 1 || len(files) > 256 {
		return "", ErrUnavailable
	}
	files = slices.Clone(files)
	slices.Sort(files)
	digest := sha256.New()
	for _, name := range files {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		fd, err := unix.Open(name, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return "", err
		}
		var info unix.Stat_t
		err = unix.Fstat(fd, &info)
		if err == nil && (info.Mode&unix.S_IFMT != unix.S_IFREG || info.Size < 0 || info.Size > 512<<20) {
			err = ErrUnavailable
		}
		if err == nil && grant != nil {
			err = grant(name, fd)
		}
		unix.Close(fd)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(digest, "%q:%d:%d:%d:%d:%d:%d:%d:%d\n", name, info.Dev, info.Ino, info.Mode, info.Size, info.Mtim.Sec, info.Mtim.Nsec, info.Ctim.Sec, info.Ctim.Nsec)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// runtimeFiles resolves only the fixed trusted tool's ELF dependencies and loader
// cache. File rules, not broad /usr/lib grants, prevent unrelated source mounts
// beneath system directories from becoming readable by a malicious media file.
func runtimeFiles() ([]string, error) { return runtimeFilesFor(profileProbe) }

func runtimeFilesFor(profile codecProfile) ([]string, error) {
	queue := []string{profileBinary(profile), dynamicLoader}
	if profile == profileImageHeader {
		for _, root := range libraryDirectories {
			for _, suffix := range []string{"vips-modules-*/vips-heif.so", "libheif/plugins/libheif-aomdec.so", "libheif/plugins/libheif-libde265.so"} {
				matches, err := filepath.Glob(filepath.Join(root, suffix))
				if err != nil || len(matches) > 64 {
					return nil, ErrUnavailable
				}
				queue = append(queue, matches...)
			}
		}
	}
	seen := make(map[string]bool)
	result := make([]string, 0, 128)
	for len(queue) > 0 {
		name, err := filepath.EvalSymlinks(queue[0])
		queue = queue[1:]
		if err != nil {
			return nil, err
		}
		if seen[name] {
			continue
		}
		if len(result) >= 256 {
			return nil, ErrUnavailable
		}
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<20 {
			return nil, ErrUnavailable
		}
		file, err := elf.Open(name)
		if err != nil {
			return nil, err
		}
		libraries, err := file.ImportedLibraries()
		paths := append([]string{}, libraryDirectories...)
		for _, tag := range []elf.DynTag{elf.DT_RUNPATH, elf.DT_RPATH} {
			entries, pathErr := file.DynString(tag)
			if pathErr != nil {
				file.Close()
				return nil, pathErr
			}
			for _, entry := range entries {
				for _, directory := range strings.Split(entry, ":") {
					directory = strings.ReplaceAll(directory, "${ORIGIN}", filepath.Dir(name))
					directory = strings.ReplaceAll(directory, "$ORIGIN", filepath.Dir(name))
					if trustedLibraryDirectory(directory) {
						paths = append(paths, directory)
					}
				}
			}
		}
		file.Close()
		if err != nil {
			return nil, err
		}
		seen[name] = true
		result = append(result, name)
		for _, library := range libraries {
			if library == "" || len(library) > 255 || filepath.Base(library) != library || strings.ContainsAny(library, "/\\\x00") {
				return nil, ErrUnavailable
			}
			found := false
			for _, directory := range paths {
				path := filepath.Join(directory, library)
				if _, err := os.Stat(path); err == nil {
					queue = append(queue, path)
					found = true
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
			}
			if !found {
				return nil, ErrUnavailable
			}
		}
	}
	if name, err := filepath.EvalSymlinks("/etc/ld.so.cache"); err == nil {
		if len(result) >= 256 {
			return nil, ErrUnavailable
		}
		result = append(result, name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return result, nil
}

func trustedLibraryDirectory(path string) bool {
	if !filepath.IsAbs(path) || strings.Contains(path, "$") {
		return false
	}
	path = filepath.Clean(path)
	for _, root := range []string{"/usr/lib", "/usr/lib64", "/lib", "/lib64"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}
