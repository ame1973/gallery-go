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
	"os"
	"path"
	"path/filepath"
	"strings"

	"gallerygo/internal/store"

	"golang.org/x/sys/unix"
)

// storeStillArtifact is worker-only private filesystem staging, not DB publication.
// The application supplies its canonical, non-overlapping cache/source configuration.
// Every write/flush/hash/rename must finish before opening a business writer.
func (s *Service) storeStillArtifact(ctx context.Context, jobID string, artifact stillArtifact, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !localPath(jobID) || strings.ContainsAny(jobID, "/\x00\r\n") || len(jobID) > 128 {
		return errStillArtifact
	}
	source := Inspection{Kind: "image", Status: "ok", Width: &artifact.width, Height: &artifact.height}
	validated, err := validateStillArtifact(ctx, artifact.revision, artifact.preset, source, data)
	if err != nil || validated != artifact {
		if err != nil {
			return err
		}
		return errStillArtifact
	}
	if !filepath.IsAbs(s.cacheRoot) || filepath.Clean(s.cacheRoot) != s.cacheRoot {
		return ErrVariantUnavailable
	}
	for _, source := range s.sources {
		if !filepath.IsAbs(source.Path) || artifactPathsOverlap(s.cacheRoot, source.Path) {
			return ErrVariantUnavailable
		}
	}
	// Do not silently follow a cache-root alias introduced after config validation.
	resolved, err := filepath.EvalSymlinks(s.cacheRoot)
	if err != nil || resolved != s.cacheRoot {
		return ErrVariantUnavailable
	}
	listed, err := os.Lstat(s.cacheRoot)
	if err != nil || !listed.IsDir() || listed.Mode().Perm()&0o022 != 0 {
		return ErrVariantUnavailable
	}
	root, err := os.OpenRoot(s.cacheRoot)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(listed, opened) {
		return ErrVariantUnavailable
	}
	if err := writeStillArtifact(ctx, root, jobID, artifact, data); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %w", ErrVariantUnavailable, err)
	}
	return ctx.Err()
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

func writeStillArtifact(ctx context.Context, root *os.Root, jobID string, artifact stillArtifact, data []byte) error {
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
	file, err := temporary.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
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
	if err := verifyArtifactFile(ctx, temporary, name, artifact); err != nil {
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
		if err := verifyArtifactFile(ctx, destination, path.Base(artifact.storage), artifact); err != nil {
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
		parent, err := current.Open(".")
		if err == nil {
			err = parent.Sync()
			parent.Close()
		}
		if err != nil {
			current.Close()
			return nil, err
		}
		next, err := current.OpenRoot(component)
		current.Close()
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

func verifyArtifactFile(ctx context.Context, root *os.Root, name string, artifact stillArtifact) error {
	listed, err := root.Lstat(name)
	if err != nil || !listed.Mode().IsRegular() || listed.Mode().Perm()&0o077 != 0 {
		return ErrVariantUnavailable
	}
	file, info, err := OpenReadOnlyFile(root, name)
	if err != nil {
		return err
	}
	defer file.Close()
	if !os.SameFile(listed, info) || info.Size() != artifact.size {
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
	after, err := root.Lstat(name)
	if err != nil || !os.SameFile(info, after) || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) || !after.Mode().IsRegular() {
		return ErrVariantUnavailable
	}
	return file.Sync()
}
