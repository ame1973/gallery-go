package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The source path belongs to this test's temporary root, never a deployed NAS.
func verifyCompiledOriginalRejectsSameStatRewrite(t *testing.T, api *scanHTTPSClient, filename, id string, expected []byte) {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil || info.Mode().Perm() != 0o400 {
		t.Fatal("source fixture must be read-only before the simulated rewrite", err)
	}
	actual, err := os.ReadFile(filename)
	if err != nil || !bytes.Equal(actual, expected) || len(expected) == 0 {
		t.Fatal("source fixture differs before rewrite", err)
	}
	path := "/media/" + id + "/original"
	var body []byte
	headers := api.request("GET", path, nil, 200, &body)
	digest := sha256.Sum256(expected)
	etag := `"sha256-` + hex.EncodeToString(digest[:]) + `"`
	if !bytes.Equal(body, expected) || headers.Get("ETag") != etag {
		t.Fatal("compiled original did not match its indexed validator", headers)
	}
	if err := os.Chmod(filename, 0o600); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() error {
		if err := os.WriteFile(filename, expected, 0o600); err != nil {
			return err
		}
		if err := os.Chtimes(filename, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
		return os.Chmod(filename, info.Mode().Perm())
	}
	defer func() {
		if !restored {
			if err := restore(); err != nil {
				t.Errorf("restore isolated source fixture: %v", err)
			}
		}
	}()
	changed := bytes.Clone(expected)
	changed[len(changed)/2] ^= 1
	if err := os.WriteFile(filename, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filename, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filename)
	if err != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		t.Fatal("fixture did not preserve inode, size and modification time", err)
	}
	for _, request := range []struct {
		method  string
		headers http.Header
	}{
		{"GET", nil},
		{"HEAD", nil},
		{"GET", http.Header{"If-None-Match": {etag}}},
		{"GET", http.Header{"If-Match": {etag}}},
		{"GET", http.Header{"Range": {"bytes=0-3"}, "If-Range": {etag}}},
		{"GET", http.Header{"Range": {"bytes=999999999-"}}},
	} {
		body = nil
		headers := api.request(request.method, path, nil, 503, &body, request.headers)
		if headers.Get("ETag") != "" || headers.Get("Cache-Control") != "private, no-store" || headers.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("compiled failure retained a stale validator or lost privacy", headers)
		}
		if request.method == "HEAD" {
			if len(body) != 0 {
				t.Fatal("HEAD failure returned a body")
			}
			continue
		}
		var problem map[string]any
		if headers.Get("Content-Type") != "application/problem+json" || json.Unmarshal(body, &problem) != nil || problem["status"] != float64(http.StatusServiceUnavailable) || problem["code"] != "SOURCE_UNAVAILABLE" || problem["request_id"] == "" || problem["request_id"] != headers.Get("X-Request-ID") {
			t.Fatal("compiled stale source failure lost its problem or correlation", problem)
		}
		delete(problem, "request_id")
		diagnostics, err := json.Marshal(problem)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{filepath.Dir(filepath.Dir(filepath.Dir(filename))), hex.EncodeToString(digest[:]), "SQL"} {
			if strings.Contains(string(diagnostics), secret) {
				t.Fatal("compiled stale source failure leaked private diagnostics")
			}
		}
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	restored = true
	body = nil
	headers = api.request("GET", path, nil, 200, &body)
	if !bytes.Equal(body, expected) || headers.Get("ETag") != etag {
		t.Fatal("restored source did not recover its indexed representation", headers)
	}
}
