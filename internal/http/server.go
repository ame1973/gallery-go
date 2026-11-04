package httpapi

import (
	"bytes"
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

	"gallerygo/internal/access"
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
	{"GET", "/admin/users", "listUsers", false},
	{"POST", "/admin/users", "createUser", false},
	{"GET", "/admin/users/{user_id}", "getUser", false},
	{"PATCH", "/admin/users/{user_id}", "updateUser", false},
	{"DELETE", "/admin/users/{user_id}", "deleteUser", false},
	{"POST", "/admin/users/{user_id}/reset-password", "resetUserPassword", false},
	{"GET", "/admin/users/{user_id}/sessions", "listUserSessions", false},
	{"DELETE", "/admin/users/{user_id}/sessions/{session_id}", "revokeUserSession", false},
	{"GET", "/admin/audit", "listAudit", false},
	{"GET", "/admin/groups", "listGroups", false},
	{"POST", "/admin/groups", "createGroup", false},
	{"GET", "/admin/groups/{group_id}", "getGroup", false},
	{"PATCH", "/admin/groups/{group_id}", "renameGroup", false},
	{"DELETE", "/admin/groups/{group_id}", "deleteGroup", false},
	{"GET", "/admin/users/{user_id}/groups", "getUserGroups", false},
	{"PUT", "/admin/users/{user_id}/groups", "setUserGroups", false},
	{"GET", "/admin/libraries", "listAdminLibraries", false},
	{"POST", "/admin/libraries", "createLibrary", false},
	{"GET", "/admin/libraries/{library_id}", "getAdminLibrary", false},
	{"PATCH", "/admin/libraries/{library_id}", "updateLibrary", false},
	{"DELETE", "/admin/libraries/{library_id}", "deleteLibrary", false},
	{"GET", "/admin/libraries/{library_id}/access", "getLibraryPolicy", false},
	{"PUT", "/admin/libraries/{library_id}/access", "setLibraryPolicy", false},
	{"GET", "/admin/albums/{album_id}/access", "getAlbumPolicy", false},
	{"PUT", "/admin/albums/{album_id}/access", "setAlbumPolicy", false},
	{"GET", "/libraries", "listLibraries", false},
	{"GET", "/libraries/{library_id}", "getLibrary", false},
	{"GET", "/albums", "listAlbums", false},
	{"GET", "/albums/{album_id}", "getAlbum", false},
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
	authorization := authorization{service: access.New(db, service)}
	handlers := map[string]http.HandlerFunc{
		"login":              authentication.login,
		"logout":             authentication.logout,
		"me":                 authentication.me,
		"getCsrf":            authentication.csrf,
		"changePassword":     authentication.password,
		"listSessions":       authentication.sessions,
		"revokeSession":      authentication.revoke,
		"listUsers":          authentication.users,
		"createUser":         authentication.createUser,
		"getUser":            authentication.user,
		"updateUser":         authentication.updateUser,
		"deleteUser":         authentication.deleteUser,
		"resetUserPassword":  authentication.resetPassword,
		"listUserSessions":   authentication.userSessions,
		"revokeUserSession":  authentication.revokeUserSession,
		"listAudit":          authentication.audit,
		"listGroups":         authorization.groups,
		"createGroup":        authorization.createGroup,
		"getGroup":           authorization.group,
		"renameGroup":        authorization.renameGroup,
		"deleteGroup":        authorization.deleteGroup,
		"getUserGroups":      authorization.userGroups,
		"setUserGroups":      authorization.setUserGroups,
		"listAdminLibraries": authorization.adminLibraries,
		"createLibrary":      authorization.createLibrary,
		"getAdminLibrary":    authorization.adminLibrary,
		"updateLibrary":      authorization.updateLibrary,
		"deleteLibrary":      authorization.deleteLibrary,
		"getLibraryPolicy":   authorization.libraryPolicy,
		"setLibraryPolicy":   authorization.setLibraryPolicy,
		"getAlbumPolicy":     authorization.albumPolicy,
		"setAlbumPolicy":     authorization.setAlbumPolicy,
		"listLibraries":      authorization.libraries,
		"getLibrary":         authorization.library,
		"listAlbums":         authorization.albums,
		"getAlbum":           authorization.album,
		"live":               func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, Health{"ok"}) },
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
	allowed := make(map[string][]string)
	for _, op := range Operations {
		handler := handlers[op.ID]
		if handler == nil {
			panic("missing API handler: " + op.ID)
		}
		if strings.HasPrefix(op.Path, "/admin/") {
			handler = authentication.admin(handler)
		}
		if !op.Public {
			handler = authentication.protect(handler)
		}
		mux.HandleFunc(op.Method+" /api/v2"+op.Path, metadataTimeout(handler))
		allowed[op.Path] = append(allowed[op.Path], op.Method)
		if op.Method == "GET" {
			allowed[op.Path] = append(allowed[op.Path], "HEAD")
		}
	}
	for route, methods := range allowed {
		mux.HandleFunc("/api/v2"+route, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", strings.Join(methods, ", "))
			problem(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "The HTTP method is not supported.")
		})
	}
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		problem(w, http.StatusNotFound, "NOT_FOUND", "The API endpoint does not exist.")
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
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
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy(""))
		next.ServeHTTP(w, r)
	})
}

func contentSecurityPolicy(nonce string) string {
	style := "style-src 'self'"
	if nonce != "" {
		style += " 'nonce-" + nonce + "'"
	}
	return "default-src 'self'; script-src 'self'; " + style + "; img-src 'self' blob: data:; media-src 'self' blob:; connect-src 'self'; font-src 'self'; worker-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
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
		// A fresh nonce and matching HTML must not be mixed through a 304 response.
		html, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
		if err != nil || len(html) > 1<<20 {
			problem(w, 503, "ASSETS_UNAVAILABLE", "The application shell could not be loaded.")
			return
		}
		nonce := rand.Text()
		html = bytes.ReplaceAll(html, []byte("__GALLERYGO_STYLE_NONCE__"), []byte(nonce))
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy(nonce))
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(html))
		return
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
