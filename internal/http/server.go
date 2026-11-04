package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"gallerygo/internal/auth"
)

// Operation is also checked against OpenAPI to prevent undocumented API routes.
type Operation struct {
	Method, Path, ID string
	Public           bool
}

var Operations = []Operation{
	{"GET", "/health/live", "live", true},
	{"GET", "/health/ready", "ready", true},
	{"GET", "/public/config", "publicConfig", true},
	{"POST", "/auth/login", "login", true},
	{"POST", "/auth/logout", "logout", false},
	{"GET", "/auth/me", "me", false},
	{"GET", "/auth/csrf", "getCsrf", false},
	{"POST", "/auth/password", "changePassword", false},
	{"GET", "/auth/sessions", "listSessions", false},
	{"DELETE", "/auth/sessions/{session_id}", "revokeSession", false},
}

type Health struct {
	Status string `json:"status"`
}

type PublicConfig struct {
	Version        string `json:"version"`
	OfflineEnabled bool   `json:"offline_enabled"`
}

// New serves only application assets and explicitly registered APIs. Private
// source files and generated media must never be mounted through this handler.
func New(db *sql.DB, assets fs.FS, version string, service *auth.Service, origin string) http.Handler {
	mux := http.NewServeMux()
	authentication := authentication{service: service, origin: origin}
	handlers := map[string]http.HandlerFunc{
		"login":          authentication.login,
		"logout":         authentication.logout,
		"me":             authentication.me,
		"getCsrf":        authentication.csrf,
		"changePassword": authentication.password,
		"listSessions":   authentication.sessions,
		"revokeSession":  authentication.revoke,
		"live":           func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, Health{"ok"}) },
		"ready": func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := db.PingContext(ctx); err != nil {
				slog.Error("readiness database failure", "request_id", w.Header().Get("X-Request-ID"), "error", err)
				problem(w, http.StatusServiceUnavailable, "NOT_READY", "The metadata database is unavailable.")
				return
			}
			writeJSON(w, http.StatusOK, Health{"ok"})
		},
		"publicConfig": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, PublicConfig{Version: version, OfflineEnabled: false})
		},
	}
	for _, op := range Operations {
		handler := handlers[op.ID]
		if !op.Public {
			handler = authentication.protect(handler)
		}
		mux.HandleFunc(op.Method+" /api/v2"+op.Path, metadataTimeout(handler))
	}
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		problem(w, http.StatusNotFound, "NOT_FOUND", "The API endpoint does not exist.")
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		for _, op := range Operations {
			if r.URL.Path == "/api/v2"+op.Path {
				allow := op.Method
				if op.Method == "GET" {
					allow += ", HEAD"
				}
				w.Header().Set("Allow", allow)
				problem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "The HTTP method is not supported.")
				return
			}
		}
		problem(w, http.StatusNotFound, "NOT_FOUND", "The API endpoint does not exist.")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { serveAsset(assets, w, r) })
	return securityHeaders(mux)
}

func metadataTimeout(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		next(w, r.WithContext(ctx))
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", rand.Text())
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' blob: data:; media-src 'self' blob:; connect-src 'self'; font-src 'self'; worker-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func serveAsset(assets fs.FS, w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		w.Header().Set("Allow", "GET, HEAD")
		problem(w, 405, "METHOD_NOT_ALLOWED", "The HTTP method is not supported.")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "api" || strings.HasPrefix(name, "api/") {
		problem(w, 404, "NOT_FOUND", "The API endpoint does not exist.")
		return
	}
	for _, segment := range strings.Split(name, "/") {
		if strings.HasPrefix(segment, ".") {
			problem(w, 404, "NOT_FOUND", "The asset does not exist.")
			return
		}
	}
	publicAsset := strings.HasPrefix(name, "assets/") || strings.HasPrefix(name, "icons/") || name == "favicon.svg" || name == "manifest.webmanifest" || name == "sw.js"
	if !publicAsset {
		if name != "index.html" && path.Ext(name) != "" {
			problem(w, 404, "NOT_FOUND", "The asset does not exist.")
			return
		}
		name = "index.html"
	}
	if assets == nil {
		problem(w, 503, "ASSETS_UNAVAILABLE", "Build and install the web assets before serving.")
		return
	}
	f, err := assets.Open(name)
	if err != nil {
		problem(w, 404, "NOT_FOUND", "The asset does not exist.")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	reader, ok := f.(io.ReadSeeker)
	if err != nil || !info.Mode().IsRegular() || !ok {
		problem(w, 404, "NOT_FOUND", "The asset does not exist.")
		return
	}
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if name == "sw.js" {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Service-Worker-Allowed", "/")
	}
	http.ServeContent(w, r, name, info.ModTime(), reader)
}
