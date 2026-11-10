//go:build linux && (amd64 || arm64)

package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"

	"gallerygo/internal/media"
)

func TestMediaHTTPAbortsOnInFlightSameSizeMtimeRewrite(t *testing.T) {
	for _, variant := range []bool{false, true} {
		name := "original"
		if variant {
			name = "variant"
		}
		t.Run(name, func(t *testing.T) {
			f := protectedMedia(t)
			session, err := f.serviceAuth(t)
			if err != nil {
				t.Fatal(err)
			}
			filename, url := f.original, f.originalURL
			var content *media.Content
			if variant {
				filename, url = f.variant, f.variantURL
				content, err = f.service.OpenVariant(t.Context(), session, "protected", "protected-variant")
			} else {
				content, err = f.service.OpenOriginal(t.Context(), session, "protected")
			}
			if err != nil {
				t.Fatal(err)
			}
			defer content.Close()
			before, err := os.Stat(filename)
			if err != nil {
				t.Fatal(err)
			}
			writer := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), writeHook: func() {
				if err := os.WriteFile(filename, bytes.Repeat([]byte{'x'}, len(f.data)), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(filename, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
				after, err := os.Stat(filename)
				if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || before.Sys().(*syscall.Stat_t).Ctim == after.Sys().(*syscall.Stat_t).Ctim {
					t.Fatal("fixture did not preserve inode/size/mtime while changing ctime", err)
				}
			}}
			aborted := false
			func() {
				defer func() {
					if value := recover(); value != nil {
						if value != http.ErrAbortHandler {
							panic(value)
						}
						aborted = true
					}
				}()
				serveMedia(writer, httptest.NewRequest("GET", url, nil), content)
			}()
			if !aborted || writer.Body.Len() >= len(f.data) || !bytes.Equal(writer.Body.Bytes(), f.data[:writer.Body.Len()]) {
				t.Fatal("same-stat rewrite produced a complete or mixed response")
			}
		})
	}
}
