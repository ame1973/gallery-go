//go:build linux && (amd64 || arm64)

package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

func stillFileIdentity(info fs.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%x:%x", uint64(stat.Dev), stat.Ino)
}

func stillFileStamp(info fs.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%x:%x:%d:%d:%d:%d:%o", uint64(stat.Dev), stat.Ino, info.Size(), info.ModTime().UnixNano(), stat.Ctim.Sec, stat.Ctim.Nsec, info.Mode())
}

func stillRootCurrent(input *stillInputFile, root SourceRoot) error {
	listed, err := os.Lstat(root.Path)
	if err != nil || !listed.IsDir() || listed.Mode()&os.ModeSymlink != 0 || !os.SameFile(listed, input.rootInfo) {
		return errStillInput
	}
	opened, err := input.root.Stat(".")
	if err != nil || !os.SameFile(opened, input.rootInfo) {
		return errStillInput
	}
	return nil
}

func readStillMarker(ctx context.Context, root *os.Root, name string) (fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, info, err := OpenReadOnlyFile(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var one [1]byte
	_, err = file.Read(one[:])
	if err != nil && err != io.EOF {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return info, nil
}

func openStillInputFile(ctx context.Context, snapshot stillInputSnapshot, configured SourceRoot, maxBytes int64) (*stillInputFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(configured.Path) || filepath.Clean(configured.Path) != configured.Path {
		return nil, errStillInput
	}
	resolved, err := filepath.EvalSymlinks(configured.Path)
	if err != nil || resolved != configured.Path {
		return nil, errStillInput
	}
	listed, err := os.Lstat(configured.Path)
	if err != nil || !listed.IsDir() || listed.Mode()&os.ModeSymlink != 0 {
		return nil, errStillInput
	}
	root, err := os.OpenRoot(configured.Path)
	if err != nil {
		return nil, err
	}
	input := &stillInputFile{root: root, rootInfo: listed}
	fail := func(err error) (*stillInputFile, error) { input.close(); return nil, err }
	if err := stillRootCurrent(input, configured); err != nil {
		return fail(err)
	}
	marker, err := readStillMarker(ctx, root, configured.Marker)
	if err != nil {
		return fail(err)
	}
	input.markerInfo = marker
	file, info, err := OpenReadOnlyFile(root, snapshot.relative)
	if err != nil {
		return fail(err)
	}
	input.file, input.fileInfo, input.stamp = file, info, stillFileStamp(info)
	if info.Size() != snapshot.size || info.ModTime().UnixNano() != snapshot.mtime || stillFileIdentity(info) != snapshot.fileIdentity || input.stamp == "" {
		return fail(errStillInput)
	}
	if err := verifyStillInputFile(ctx, input, snapshot, configured, maxBytes); err != nil {
		return fail(err)
	}
	return input, nil
}

func verifyStillInputFile(ctx context.Context, input *stillInputFile, snapshot stillInputSnapshot, configured SourceRoot, maxBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if input == nil || input.file == nil || input.root == nil || snapshot.size < 1 || snapshot.size > maxBytes {
		return errStillInput
	}
	if err := stillRootCurrent(input, configured); err != nil {
		return err
	}
	marker, err := readStillMarker(ctx, input.root, configured.Marker)
	if err != nil {
		return err
	}
	if !os.SameFile(marker, input.markerInfo) || stillFileStamp(marker) != stillFileStamp(input.markerInfo) {
		return errStillInput
	}
	before, err := input.file.Stat()
	if err != nil || !before.Mode().IsRegular() || stillFileStamp(before) != input.stamp {
		return errStillInput
	}
	if _, err := input.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(inspectReader{ctx, input.file}, snapshot.size+1))
	if err != nil {
		return err
	}
	if count != snapshot.size || hex.EncodeToString(digest.Sum(nil)) != snapshot.hash {
		return errStillInput
	}
	after, err := input.file.Stat()
	if err != nil || stillFileStamp(after) != input.stamp {
		return errStillInput
	}
	current, currentInfo, err := OpenReadOnlyFile(input.root, snapshot.relative)
	if err != nil {
		return err
	}
	current.Close()
	if !os.SameFile(currentInfo, input.fileInfo) || stillFileStamp(currentInfo) != input.stamp {
		return errStillInput
	}
	if err := stillRootCurrent(input, configured); err != nil {
		return err
	}
	marker, err = readStillMarker(ctx, input.root, configured.Marker)
	if err != nil {
		return err
	}
	if !os.SameFile(marker, input.markerInfo) || stillFileStamp(marker) != stillFileStamp(input.markerInfo) {
		return errStillInput
	}
	if _, err := input.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return ctx.Err()
}
