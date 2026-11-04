package scanner

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"

	"gallerygo/internal/library"
	"gallerygo/internal/media"
)

const maxRelativePath = 4096
const hashChunk = 64 << 10

func validRelative(name string) bool {
	return name != "." && len(name) <= maxRelativePath && fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00")
}

func fileIdentity(info fs.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return strconv.FormatUint(uint64(stat.Dev), 16) + ":" + strconv.FormatUint(stat.Ino, 16)
}

func sameSignature(before, after fs.FileInfo) bool {
	return os.SameFile(before, after) && before.Mode().Type() == after.Mode().Type() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

type directoryStamp struct {
	count  int64
	digest [sha256.Size]byte
}

func (stamp *directoryStamp) add(entry fs.DirEntry) {
	// Order-independent because ReadDir order is not a portable filesystem promise.
	digest := sha256.Sum256([]byte(entry.Name() + "\x00" + entry.Type().String()))
	for index := range digest {
		stamp.digest[index] ^= digest[index]
	}
	stamp.count++
}

type walk struct {
	ctx     context.Context
	root    *os.Root
	marker  fs.FileInfo
	policy  Policy
	yield   func(File) error
	summary Summary
	buffer  []byte
}

// Walk enumerates directories in bounded ReadDir batches and streams SHA-256 in
// fixed chunks. Enumeration order is filesystem-dependent; staging sorts by path
// for deterministic publication instead of loading an entire directory to sort.
// Every regular path is represented, including duplicate/hard-linked content.
func Walk(ctx context.Context, configured media.SourceRoot, policy Policy, yield func(File) error) (Summary, error) {
	if policy.Validate() != nil || yield == nil {
		return Summary{}, ErrPolicy
	}
	ctx, cancel := context.WithTimeout(ctx, policy.Timeout)
	defer cancel()
	if err := library.ProbeRoot(ctx, configured); err != nil {
		return Summary{}, err
	}
	root, err := os.OpenRoot(configured.Path)
	if err != nil {
		return Summary{}, err
	}
	defer root.Close()
	marker, err := root.Stat(configured.Marker)
	if err != nil || !marker.Mode().IsRegular() {
		if err != nil {
			return Summary{}, err
		}
		return Summary{}, ErrUnsafeEntry
	}
	walker := walk{ctx: ctx, root: root, marker: marker, policy: policy, yield: yield, buffer: make([]byte, hashChunk)}
	if err := walker.directory(".", nil); err != nil {
		return walker.summary, err
	}
	if err := library.ProbeRoot(ctx, configured); err != nil {
		return walker.summary, err
	}
	return walker.summary, nil
}

func (w *walk) directory(relative string, ancestors []fs.FileInfo) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if len(ancestors) > w.policy.MaxDepth {
		return &PathError{relative, ErrLimit}
	}
	folder, err := w.root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		return &PathError{relative, err}
	}
	defer folder.Close()
	before, err := folder.Stat()
	if err != nil {
		return &PathError{relative, err}
	}
	if !before.IsDir() {
		return &PathError{relative, ErrChanged}
	}
	for _, ancestor := range ancestors {
		if os.SameFile(ancestor, before) {
			return &PathError{relative, ErrCycle}
		}
	}
	ancestors = append(ancestors, before)
	w.summary.Directories++
	var stamp directoryStamp
	for {
		entries, err := folder.ReadDir(w.policy.BatchSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return &PathError{relative, err}
		}
		for _, entry := range entries {
			stamp.add(entry)
			if err := w.ctx.Err(); err != nil {
				return err
			}
			name := path.Join(relative, entry.Name())
			if !validRelative(name) {
				return &PathError{name, ErrUnsafeEntry}
			}
			info, err := w.root.Stat(name)
			if err != nil {
				return &PathError{name, err}
			} // os.Root rejects escaping links.
			if info.IsDir() {
				if err := w.directory(name, ancestors); err != nil {
					return err
				}
				continue
			}
			if !info.Mode().IsRegular() {
				return &PathError{name, ErrUnsafeEntry}
			}
			if os.SameFile(info, w.marker) {
				continue
			}
			if w.summary.Files >= w.policy.MaxFiles {
				return &PathError{name, ErrLimit}
			}
			file, err := w.hashFile(name, info)
			if err != nil {
				return &PathError{name, err}
			}
			if file.SizeBytes > math.MaxInt64-w.summary.Bytes {
				return &PathError{name, ErrLimit}
			}
			if err := w.yield(file); err != nil {
				return err
			}
			w.summary.Files++
			w.summary.Bytes += file.SizeBytes
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	after, err := folder.Stat()
	if err != nil {
		return &PathError{relative, err}
	}
	current, err := w.root.Stat(relative)
	if err != nil {
		return &PathError{relative, err}
	}
	if !sameSignature(before, after) || !sameSignature(before, current) {
		return &PathError{relative, ErrChanged}
	}
	return w.verifyDirectory(relative, before, stamp)
}

func (w *walk) verifyDirectory(relative string, before fs.FileInfo, expected directoryStamp) error {
	// Coarse directory timestamps and unchanged allocation size can hide an insert.
	// Re-enumerate bounded names/types rather than trusting stat alone.
	folder, err := w.root.OpenFile(relative, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_DIRECTORY, 0)
	if err != nil {
		return &PathError{relative, err}
	}
	defer folder.Close()
	var current directoryStamp
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		entries, err := folder.ReadDir(w.policy.BatchSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return &PathError{relative, err}
		}
		for _, entry := range entries {
			current.add(entry)
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	after, err := folder.Stat()
	if err != nil {
		return &PathError{relative, err}
	}
	if current != expected || !sameSignature(before, after) {
		return &PathError{relative, ErrChanged}
	}
	return nil
}

func (w *walk) hashFile(name string, listed fs.FileInfo) (File, error) {
	if listed.Size() < 0 || listed.Size() > w.policy.MaxFileBytes {
		return File{}, ErrLimit
	}
	file, before, err := media.OpenReadOnlyFile(w.root, name)
	if err != nil {
		return File{}, err
	}
	defer file.Close()
	if !sameSignature(listed, before) {
		return File{}, ErrChanged
	}
	hash, quick := sha256.New(), sha256.New()
	var read int64
	for {
		if err := w.ctx.Err(); err != nil {
			return File{}, err
		}
		n, err := file.Read(w.buffer)
		if n > 0 {
			if int64(n) > w.policy.MaxFileBytes-read {
				return File{}, ErrLimit
			}
			hash.Write(w.buffer[:n])
			if read < hashChunk {
				take := min(int64(n), hashChunk-read)
				quick.Write(w.buffer[:take])
			}
			read += int64(n)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return File{}, err
		}
	}
	after, err := file.Stat()
	if err != nil {
		return File{}, err
	}
	current, err := w.root.Stat(name)
	if err != nil {
		return File{}, err
	}
	if read != before.Size() || !sameSignature(before, after) || !sameSignature(before, current) {
		return File{}, ErrChanged
	}
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(read))
	quick.Write(size[:])
	return File{RelativePath: name, SizeBytes: read, MtimeNS: before.ModTime().UnixNano(), Identity: fileIdentity(before), QuickFingerprint: hex.EncodeToString(quick.Sum(nil)), SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}
