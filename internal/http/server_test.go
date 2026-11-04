package httpapi

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/contracts"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
	"gallerygo/migrations"

	"go.yaml.in/yaml/v3"
)

func newTestServer(t *testing.T) (*sql.DB, http.Handler) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	assets := fstest.MapFS{
		"index.html":        {Data: []byte("<!doctype html><title>GalleryGo</title>")},
		"assets/app-abc.js": {Data: []byte("console.log('app');")},
		"icons/logo.svg":    {Data: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")},
		".env":              {Data: []byte("SECRET_VALUE")},
	}
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return db, New(db, assets, "test-version", service, "https://gallery.test")
}

func TestHealthAndHeaders(t *testing.T) {
	db, h := newTestServer(t)
	for _, path := range []string{"/api/v2/health/live", "/api/v2/health/ready", "/api/v2/public/config"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("X-Request-ID", "untrusted-id")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		for _, header := range []string{"X-Request-ID", "Cache-Control", "X-Content-Type-Options", "Referrer-Policy", "Content-Security-Policy", "Permissions-Policy"} {
			if w.Header().Get(header) == "" {
				t.Errorf("missing %s", header)
			}
		}
		if w.Header().Get("X-Request-ID") == "untrusted-id" {
			t.Error("trusted client request ID")
		}
		if w.Header().Get("Cache-Control") != "private, no-store" {
			t.Error("API cacheable")
		}
		if strings.Contains(w.Body.String(), "test.db") {
			t.Error("private path leaked")
		}
	}
	db.Close()
	for path, want := range map[string]int{"/api/v2/health/live": 200, "/api/v2/health/ready": 503} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != want {
			t.Errorf("%s after database failure: %d", path, w.Code)
		}
		if want == 503 {
			assertProblem(t, w, want)
		}
	}
}

func assertProblem(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != status || p.Code == "" || p.RequestID == "" || p.RequestID != w.Header().Get("X-Request-ID") {
		t.Fatalf("invalid problem: %+v", p)
	}
	if w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatal("incorrect problem MIME")
	}
}

func TestUnknownRoutesAndMethods(t *testing.T) {
	_, h := newTestServer(t)
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/v2/albums", 401}, {"GET", "/api/v2/unknown-resource", 404}, {"GET", "/api/v1/albums", 404}, {"GET", "/api", 404},
		{"POST", "/api/v2/health/live", 405}, {"POST", "/", 405},
		{"GET", "/.env", 404}, {"GET", "/.gallerygo/image.webp", 404}, {"GET", "/missing.js", 404},
		{"GET", "/assets/", 404}, {"GET", "/assets/.secret", 404},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
			if w.Code != c.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			assertProblem(t, w, c.status)
		})
	}
}

func TestAssetsAndSPAFallback(t *testing.T) {
	_, h := newTestServer(t)
	for _, path := range []string{"/", "/albums/example", "/assets/app-abc.js", "/icons/logo.svg"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if path == "/assets/app-abc.js" && !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Fatal("hashed asset is not immutable")
		}
	}
	server := httptest.NewServer(h)
	defer server.Close()
	response, err := server.Client().Head(server.URL + "/assets/app-abc.js")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || len(body) != 0 || response.StatusCode != 200 {
		t.Fatalf("HEAD status=%d bytes=%d error=%v", response.StatusCode, len(body), err)
	}
}

func TestAssetRootRejectsSymlinkEscape(t *testing.T) {
	db, _ := newTestServer(t)
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private.js")
	if err := os.WriteFile(outside, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "assets", "escape.js")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	w := httptest.NewRecorder()
	service, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	New(db, root.FS(), "test", service, "https://gallery.test").ServeHTTP(w, httptest.NewRequest("GET", "/assets/escape.js", nil))
	if w.Code != 404 || strings.Contains(w.Body.String(), "PRIVATE") {
		t.Fatalf("escape: %d %s", w.Code, w.Body.String())
	}
}

func TestOpenAPIRegisteredOperationsAndResponses(t *testing.T) {
	var spec struct {
		OpenAPI    string                          `yaml:"openapi"`
		Paths      map[string]map[string]yaml.Node `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Required   []string       `yaml:"required"`
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(contracts.OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.OpenAPI != "3.1.0" {
		t.Fatal("expected OpenAPI 3.1")
	}
	_, h := newTestServer(t)
	count := 0
	for _, methods := range spec.Paths {
		for method := range methods {
			switch method {
			case "get", "head", "post", "put", "patch", "delete", "options", "trace":
				count++
			}
		}
	}
	if count != len(Operations) {
		t.Fatalf("spec operations=%d registered=%d", count, len(Operations))
	}
	for _, op := range Operations {
		var operation struct {
			OperationID string `yaml:"operationId"`
		}
		node := spec.Paths[op.Path][strings.ToLower(op.Method)]
		if err := node.Decode(&operation); err != nil {
			t.Fatal(err)
		}
		if operation.OperationID != op.ID {
			t.Errorf("operation missing from contract: %+v", op)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(op.Method, "/api/v2"+op.Path, nil))
		if !op.Public || op.ID == "login" {
			want := http.StatusUnauthorized
			if op.ID == "login" {
				want = http.StatusForbidden
			}
			if w.Code != want {
				t.Errorf("unauthenticated %s status=%d", op.ID, w.Code)
			}
			assertProblem(t, w, want)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		schemaName := "Health"
		if op.ID == "publicConfig" {
			schemaName = "PublicConfig"
		}
		schema := spec.Components.Schemas[schemaName]
		for _, key := range schema.Required {
			if _, ok := body[key]; !ok {
				t.Errorf("%s missing required %s", op.ID, key)
			}
		}
		for key := range body {
			if _, ok := schema.Properties[key]; !ok {
				t.Errorf("%s unknown response field %s", op.ID, key)
			}
		}
	}
}
