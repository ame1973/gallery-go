package httpapi

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMediaHTTPRejectsSameStatBytesChangedBeforeOpen(t *testing.T) {
	for _, variant := range []bool{false, true} {
		for _, replace := range []bool{false, true} {
			kind, operation := "original", "rewrite"
			if variant {
				kind = "variant"
			}
			if replace {
				operation = "replacement"
			}
			t.Run(kind+"/"+operation, func(t *testing.T) {
				f := protectedMedia(t)
				filename, endpoint, code := f.original, f.originalURL, "SOURCE_UNAVAILABLE"
				if variant {
					filename, endpoint, code = f.variant, f.variantURL, "VARIANT_UNAVAILABLE"
				}
				before, err := os.Stat(filename)
				if err != nil {
					t.Fatal(err)
				}
				changed := bytes.Clone(f.data)
				changed[len(changed)/2] ^= 1
				target := filename
				if replace {
					target = filepath.Join(filepath.Dir(filename), "isolated-replacement")
				}
				if err := os.WriteFile(target, changed, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
				if replace {
					if err := os.Rename(target, filename); err != nil {
						t.Fatal(err)
					}
				}
				after, err := os.Stat(filename)
				if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || os.SameFile(before, after) == replace {
					t.Fatal("fixture did not preserve size/mtime and the intended inode relationship", err)
				}
				etag := `"sha256-` + f.hash + `"`
				for _, request := range []struct {
					name, method string
					headers      http.Header
				}{
					{"get", "GET", nil},
					{"head", "HEAD", nil},
					{"not-modified", "GET", http.Header{"If-None-Match": {etag}}},
					{"matching", "GET", http.Header{"If-Match": {etag}}},
					{"range", "GET", http.Header{"Range": {"bytes=0-15"}, "If-Range": {etag}}},
					{"unsatisfiable", "GET", http.Header{"Range": {"bytes=999999999-"}}},
				} {
					response := mediaRequest(f.reader, request.method, endpoint, request.headers)
					if response.Code != http.StatusServiceUnavailable {
						t.Errorf("%s returned status %d and ETag %q for bytes that do not match the indexed hash", request.name, response.Code, response.Header().Get("ETag"))
						continue
					}
					if request.method != "HEAD" {
						expectCode(t, response, http.StatusServiceUnavailable, code)
						for _, secret := range []string{f.source, f.cache, f.hash, "SQL"} {
							if strings.Contains(problemContent(t, response), secret) {
								t.Fatal("unavailable response exposed private diagnostics", secret)
							}
						}
					}
					if response.Header().Get("ETag") != "" {
						t.Fatal("unavailable response retained a stale validator", response.Header())
					}
					if response.Header().Get("Cache-Control") != "private, no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
						t.Fatal("unavailable response lost private headers", response.Header())
					}
				}
			})
		}
	}
}
