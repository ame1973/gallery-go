package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
)

func (s *Service) acquireContentVerification(ctx context.Context) error {
	select {
	case s.contentVerifies <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sameContentInfo(before, after fs.FileInfo) bool {
	return after.Mode().IsRegular() && os.SameFile(before, after) && after.Size() == before.Size() && after.ModTime().Equal(before.ModTime()) && !contentChangeTimeChanged(before, after)
}

func (content *Content) sameInfo(info fs.FileInfo) bool {
	return sameContentInfo(content.info, info)
}

func readContentMarker(ctx context.Context, root *os.Root, name string) (fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, info, err := OpenReadOnlyFile(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var one [1]byte
	if _, err := file.Read(one[:]); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return info, nil
}

// sameNamedFile detects replacement or removal of the rooted name during the
// hash without following a path outside the already-confined root.
func (content *Content) sameNamedFile(root *os.Root, name string) error {
	if err := content.ctx.Err(); err != nil {
		return err
	}
	file, info, err := OpenReadOnlyFile(root, name)
	if err != nil {
		return err
	}
	defer file.Close()
	if !content.sameInfo(info) {
		return ErrContentChanged
	}
	return content.check()
}

// verifyDigest checks the complete opened representation before its strong ETag
// or any conditional response is returned. The caller owns the descriptor and
// must close it on error. Reads remain bounded and honor request cancellation.
func (content *Content) verifyDigest(expected string) error {
	if err := content.check(); err != nil {
		return err
	}
	if _, err := content.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest := sha256.New()
	buffer := make([]byte, 256<<10)
	count, err := io.CopyBuffer(digest, io.LimitReader(inspectReader{content.ctx, content.file}, content.Size), buffer)
	if err != nil {
		return err
	}
	if err := content.check(); err != nil {
		return err
	}
	if count != content.Size || hex.EncodeToString(digest.Sum(nil)) != expected {
		return ErrContentChanged
	}
	if _, err := content.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return content.check()
}
