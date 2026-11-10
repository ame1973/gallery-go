//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

func TestContentRejectsInFlightSameSizeMtimeRewrite(t *testing.T) {
	for _, variant := range []bool{false, true} {
		name := "original"
		if variant {
			name = "variant"
		}
		t.Run(name, func(t *testing.T) {
			f := fixture(t)
			filename := f.original
			var content *Content
			var err error
			if variant {
				filename = f.variant
				content, err = f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
			} else {
				content, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer content.Close()
			first := make([]byte, 1)
			if _, err := io.ReadFull(content, first); err != nil || first[0] != f.data[0] {
				t.Fatal("initial stream read failed", err)
			}
			before, err := os.Stat(filename)
			if err != nil {
				t.Fatal(err)
			}
			changed := bytes.Clone(f.data)
			changed[len(changed)-1] ^= 1
			if err := os.WriteFile(filename, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(filename, before.ModTime(), before.ModTime()); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(filename)
			if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || stillFileStamp(before) == stillFileStamp(after) {
				t.Fatal("fixture did not retain inode/size/mtime while changing its ctime", err)
			}
			if n, err := content.Read(make([]byte, len(f.data))); n != 0 || !errors.Is(err, ErrContentChanged) || !errors.Is(content.Err(), ErrContentChanged) {
				t.Fatal("same-stat rewrite was returned as an unchanged stream", n, err, content.Err())
			}
			if _, err := content.Seek(0, io.SeekStart); !errors.Is(err, ErrContentChanged) {
				t.Fatal("same-stat rewrite was seekable as an unchanged stream", err)
			}
		})
	}
}
