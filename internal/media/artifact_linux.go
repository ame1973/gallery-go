//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gallerygo/internal/store"

	"golang.org/x/sys/unix"
)

// storeStillArtifact is private filesystem staging, not DB publication. The returned
// capsule retains shared exclusion against future orphan reconciliation; keep it
// open through the final fenced DB publication, then Close on every outcome.
// Every write/flush/hash/rename must finish before opening a business writer.
func (s *Service) storeStillArtifact(ctx context.Context, jobID string, artifact stillArtifact, data []byte) (*storedStillArtifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !localPath(jobID) || strings.ContainsAny(jobID, "/\x00\r\n") || len(jobID) > 128 {
		return nil, errStillArtifact
	}
	source := Inspection{Kind: "image", Status: "ok", Width: &artifact.width, Height: &artifact.height}
	validated, err := validateStillArtifact(ctx, artifact.revision, artifact.preset, source, data)
	if err != nil || validated != artifact {
		if err != nil {
			return nil, err
		}
		return nil, errStillArtifact
	}
	gate, err := s.lockArtifacts(ctx, false)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			gate.Close()
		}
	}()
	if err := writeStillArtifact(ctx, gate.root, gate.rootInfo, jobID, artifact, data); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	if err := gate.check(ctx); err != nil {
		return nil, err
	}
	keep = true
	return &storedStillArtifact{artifact: artifact, jobID: jobID, gate: gate}, nil
}

func artifactPathsOverlap(a, b string) bool {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && (relative == "." || filepath.IsLocal(relative)) {
			return true
		}
	}
	return false
}

func writeStillArtifact(ctx context.Context, root *os.Root, cacheInfo fs.FileInfo, jobID string, artifact stillArtifact, data []byte) error {
	temporaryPath := path.Join("tmp", jobID)
	temporary, err := makeArtifactDirectory(root, temporaryPath)
	if err != nil {
		return err
	}
	// Keep the job directory for lease-aware orphan GC. Removing an empty folder
	// here could detach another retry's already-pinned directory before it writes.
	defer temporary.Close()
	destination, err := makeArtifactDirectory(root, path.Dir(artifact.storage))
	if err != nil {
		return err
	}
	defer destination.Close()
	name := store.NewID() + ".tmp"
	file, err := openArtifactAt(temporary, name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	defer func() { _ = temporary.Remove(name) }()
	count, err := io.Copy(file, inspectReader{ctx, bytes.NewReader(data)})
	if err != nil {
		return err
	}
	if count != artifact.size {
		return errStillArtifact
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := verifyArtifactFile(ctx, temporary, cacheInfo, name, artifact); err != nil {
		return err
	}
	from, err := temporary.Open(".")
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := destination.Open(".")
	if err != nil {
		return err
	}
	defer to.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Root.Rename replaces existing files. Pin BOTH directories with os.Root, then
	// pass only generated basenames to the kernel's atomic no-replace operation.
	// Never use multi-component raw renameat paths that could follow a symlink out.
	err = unix.Renameat2(int(from.Fd()), name, int(to.Fd()), path.Base(artifact.storage), unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EEXIST) {
		if err := verifyArtifactFile(ctx, destination, cacheInfo, path.Base(artifact.storage), artifact); err != nil {
			return err
		}
		if err := temporary.Remove(name); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := to.Sync(); err != nil {
		return err
	}
	return from.Sync()
}

// Resolve one generated basename relative to an already-pinned directory. A mount
// point or symlink introduced after earlier checks must not redirect cache I/O.
func openArtifactAt(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	if !localPath(name) || strings.Contains(name, "/") {
		return nil, ErrVariantUnavailable
	}
	parent, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	how := unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Mode:    uint64(mode.Perm()),
		Resolve: unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(int(parent.Fd()), name, &how)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// OpenRoot has no no-cross-device option. Reopen only the already-pinned child
// descriptor, then verify the resulting Root still names that same directory.
func openArtifactDirectory(root *os.Root, name string) (*os.Root, fs.FileInfo, error) {
	file, err := openArtifactAt(root, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.IsDir() {
		return nil, nil, ErrVariantUnavailable
	}
	child, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", file.Fd()))
	if err != nil {
		return nil, nil, err
	}
	current, err := child.Stat(".")
	if err != nil || !os.SameFile(info, current) || !current.IsDir() {
		child.Close()
		return nil, nil, ErrVariantUnavailable
	}
	return child, current, nil
}

func makeArtifactDirectory(root *os.Root, relative string) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(relative, "/") {
		if !localPath(component) || strings.Contains(component, "/") {
			current.Close()
			return nil, errStillArtifact
		}
		if err := current.Mkdir(component, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			current.Close()
			return nil, err
		}
		info, err := current.Lstat(component)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
			current.Close()
			return nil, ErrVariantUnavailable
		}
		next, opened, err := openArtifactDirectory(current, component)
		if err != nil || !os.SameFile(info, opened) || opened.Mode().Perm()&0o022 != 0 {
			if next != nil {
				next.Close()
			}
			current.Close()
			return nil, ErrVariantUnavailable
		}
		parent, err := current.Open(".")
		if err == nil {
			err = parent.Sync()
			parent.Close()
		}
		current.Close()
		if err != nil {
			next.Close()
			return nil, err
		}
		current = next
	}
	return current, nil
}

// Use the reconciliation identity policy for both temporary and replayed files.
// Change-time checks detect observed mutations, not uncooperative same-UID writers.
func verifyArtifactFile(ctx context.Context, root *os.Root, cacheInfo fs.FileInfo, name string, artifact stillArtifact) error {
	listed, err := root.Lstat(name)
	if err != nil || !artifactTempPrivate(listed, cacheInfo, false) {
		return ErrVariantUnavailable
	}
	file, err := openArtifactAt(root, name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !os.SameFile(listed, info) || !artifactTempPrivate(info, cacheInfo, false) || stillFileStamp(listed) != stillFileStamp(info) || info.Size() != artifact.size {
		return ErrVariantUnavailable
	}
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(inspectReader{ctx, file}, artifact.size+1))
	if err != nil {
		return err
	}
	if count != artifact.size || hex.EncodeToString(digest.Sum(nil)) != artifact.hash {
		return ErrVariantUnavailable
	}
	current, err := file.Stat()
	if err != nil || !artifactTempPrivate(current, cacheInfo, false) || !os.SameFile(info, current) || stillFileStamp(info) != stillFileStamp(current) {
		return ErrVariantUnavailable
	}
	after, err := root.Lstat(name)
	if err != nil || !artifactTempPrivate(after, cacheInfo, false) || !os.SameFile(info, after) || stillFileStamp(info) != stillFileStamp(after) {
		return ErrVariantUnavailable
	}
	return file.Sync()
}
