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
	"slices"
	"strings"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/health"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/scanner"
)

// Operation is also checked against OpenAPI to prevent undocumented API routes.
type Operation struct {
	Method, Path, ID  string
	Public, Streaming bool
}

var Operations = []Operation{
	{Method: "GET", Path: "/health/live", ID: "live", Public: true},
	{Method: "GET", Path: "/health/ready", ID: "ready", Public: true},
	{Method: "GET", Path: "/public/config", ID: "publicConfig", Public: true},
	{Method: "POST", Path: "/auth/login", ID: "login", Public: true},
	{Method: "POST", Path: "/auth/logout", ID: "logout"},
	{Method: "GET", Path: "/auth/me", ID: "me"},
	{Method: "GET", Path: "/auth/csrf", ID: "getCsrf"},
	{Method: "POST", Path: "/auth/password", ID: "changePassword"},
	{Method: "GET", Path: "/auth/sessions", ID: "listSessions"},
	{Method: "DELETE", Path: "/auth/sessions/{session_id}", ID: "revokeSession"},
	{Method: "GET", Path: "/admin/users", ID: "listUsers"},
	{Method: "POST", Path: "/admin/users", ID: "createUser"},
	{Method: "GET", Path: "/admin/users/{user_id}", ID: "getUser"},
	{Method: "PATCH", Path: "/admin/users/{user_id}", ID: "updateUser"},
	{Method: "DELETE", Path: "/admin/users/{user_id}", ID: "deleteUser"},
	{Method: "POST", Path: "/admin/users/{user_id}/reset-password", ID: "resetUserPassword"},
	{Method: "GET", Path: "/admin/users/{user_id}/sessions", ID: "listUserSessions"},
	{Method: "DELETE", Path: "/admin/users/{user_id}/sessions/{session_id}", ID: "revokeUserSession"},
	{Method: "GET", Path: "/admin/audit", ID: "listAudit"},
	{Method: "GET", Path: "/admin/duplicates/exact", ID: "listExactDuplicateGroups"},
	{Method: "GET", Path: "/admin/duplicates/exact/{group_id}/media", ID: "listExactDuplicateMedia"},
	{Method: "GET", Path: "/admin/source-roots", ID: "listSourceRoots"},
	{Method: "GET", Path: "/admin/sources", ID: "listSources"},
	{Method: "POST", Path: "/admin/sources", ID: "createSource"},
	{Method: "GET", Path: "/admin/sources/{source_id}", ID: "getSource"},
	{Method: "PATCH", Path: "/admin/sources/{source_id}", ID: "updateSource"},
	{Method: "DELETE", Path: "/admin/sources/{source_id}", ID: "deleteSource"},
	{Method: "POST", Path: "/admin/sources/{source_id}/check", ID: "checkSource"},
	{Method: "POST", Path: "/admin/scans", ID: "enqueueScan"},
	{Method: "POST", Path: "/admin/batches/tags", ID: "enqueueTagBatch"},
	{Method: "POST", Path: "/admin/batches/covers", ID: "enqueueCoverBatch"},
	{Method: "GET", Path: "/admin/batches/{job_id}/covers", ID: "listCoverBatchTargets"},
	{Method: "GET", Path: "/admin/batches/{job_id}/targets", ID: "listTagBatchTargets"},
	{Method: "GET", Path: "/admin/batches/{job_id}/results", ID: "listTagBatchResults"},
	{Method: "GET", Path: "/admin/jobs", ID: "listJobs"},
	{Method: "GET", Path: "/admin/jobs/{job_id}", ID: "getJob"},
	{Method: "GET", Path: "/admin/jobs/{job_id}/attempts", ID: "listJobAttempts"},
	{Method: "GET", Path: "/admin/jobs/{job_id}/issues", ID: "listJobIssues"},
	{Method: "POST", Path: "/admin/jobs/{job_id}/cancel", ID: "cancelJob"},
	{Method: "POST", Path: "/admin/jobs/{job_id}/retry", ID: "retryJob"},
	{Method: "GET", Path: "/admin/tags", ID: "listTags"},
	{Method: "POST", Path: "/admin/tags", ID: "createTag"},
	{Method: "GET", Path: "/admin/tags/{tag_id}", ID: "getTag"},
	{Method: "PATCH", Path: "/admin/tags/{tag_id}", ID: "renameTag"},
	{Method: "DELETE", Path: "/admin/tags/{tag_id}", ID: "deleteTag"},
	{Method: "GET", Path: "/admin/groups", ID: "listGroups"},
	{Method: "POST", Path: "/admin/groups", ID: "createGroup"},
	{Method: "GET", Path: "/admin/groups/{group_id}", ID: "getGroup"},
	{Method: "PATCH", Path: "/admin/groups/{group_id}", ID: "renameGroup"},
	{Method: "DELETE", Path: "/admin/groups/{group_id}", ID: "deleteGroup"},
	{Method: "GET", Path: "/admin/users/{user_id}/groups", ID: "getUserGroups"},
	{Method: "PUT", Path: "/admin/users/{user_id}/groups", ID: "setUserGroups"},
	{Method: "GET", Path: "/admin/libraries", ID: "listAdminLibraries"},
	{Method: "POST", Path: "/admin/libraries", ID: "createLibrary"},
	{Method: "GET", Path: "/admin/libraries/{library_id}", ID: "getAdminLibrary"},
	{Method: "PATCH", Path: "/admin/libraries/{library_id}", ID: "updateLibrary"},
	{Method: "DELETE", Path: "/admin/libraries/{library_id}", ID: "deleteLibrary"},
	{Method: "GET", Path: "/admin/libraries/{library_id}/access", ID: "getLibraryPolicy"},
	{Method: "PUT", Path: "/admin/libraries/{library_id}/access", ID: "setLibraryPolicy"},
	{Method: "GET", Path: "/admin/albums/{album_id}", ID: "getAdminAlbumMetadata"},
	{Method: "PATCH", Path: "/admin/albums/{album_id}", ID: "updateAlbumMetadata"},
	{Method: "GET", Path: "/admin/albums/{album_id}/creator", ID: "getAlbumCreator"},
	{Method: "PUT", Path: "/admin/albums/{album_id}/creator", ID: "setAlbumCreator"},
	{Method: "GET", Path: "/admin/albums/{album_id}/tags", ID: "getAdminAlbumTags"},
	{Method: "PUT", Path: "/admin/albums/{album_id}/tags", ID: "setAlbumTags"},
	{Method: "GET", Path: "/admin/albums/{album_id}/access", ID: "getAlbumPolicy"},
	{Method: "PUT", Path: "/admin/albums/{album_id}/access", ID: "setAlbumPolicy"},
	{Method: "GET", Path: "/libraries", ID: "listLibraries"},
	{Method: "GET", Path: "/libraries/{library_id}", ID: "getLibrary"},
	{Method: "GET", Path: "/admin/people/{person_id}", ID: "getAdminPersonMetadata"},
	{Method: "PATCH", Path: "/admin/people/{person_id}", ID: "updatePersonMetadata"},
	{Method: "GET", Path: "/people", ID: "listPeople"},
	{Method: "GET", Path: "/people/{person_id}", ID: "getPerson"},
	{Method: "GET", Path: "/people/{person_id}/albums", ID: "listPersonAlbums"},
	{Method: "GET", Path: "/albums", ID: "listAlbums"},
	{Method: "GET", Path: "/albums/{album_id}", ID: "getAlbum"},
	{Method: "GET", Path: "/favorite-collections", ID: "listFavoriteCollections"},
	{Method: "POST", Path: "/favorite-collections", ID: "createFavoriteCollection"},
	{Method: "GET", Path: "/favorite-collections/{collection_id}", ID: "getFavoriteCollection"},
	{Method: "PATCH", Path: "/favorite-collections/{collection_id}", ID: "renameFavoriteCollection"},
	{Method: "GET", Path: "/favorites/{album_id}", ID: "getAlbumFavorite"},
	{Method: "PUT", Path: "/favorites/{album_id}", ID: "setAlbumFavorite"},
	{Method: "DELETE", Path: "/favorites/{album_id}", ID: "deleteAlbumFavorite"},
	{Method: "GET", Path: "/albums/{album_id}/rating", ID: "getAlbumRating"},
	{Method: "PUT", Path: "/albums/{album_id}/rating", ID: "setAlbumRating"},
	{Method: "DELETE", Path: "/albums/{album_id}/rating", ID: "deleteAlbumRating"},
	{Method: "GET", Path: "/albums/{album_id}/views", ID: "getAlbumView"},
	{Method: "POST", Path: "/albums/{album_id}/views", ID: "recordAlbumView"},
	{Method: "GET", Path: "/albums/{album_id}/tags", ID: "listAlbumTags"},
	{Method: "GET", Path: "/albums/{album_id}/media", ID: "listAlbumMedia"},
	{Method: "GET", Path: "/media/{media_id}", ID: "getMedia"},
	{Method: "GET", Path: "/media/{media_id}/variants", ID: "listVariants"},
	{Method: "GET", Path: "/media/{media_id}/variants/{variant_id}/content", ID: "getVariantContent", Streaming: true},
	{Method: "HEAD", Path: "/media/{media_id}/variants/{variant_id}/content", ID: "headVariantContent", Streaming: true},
	{Method: "GET", Path: "/media/{media_id}/original", ID: "getOriginal", Streaming: true},
	{Method: "HEAD", Path: "/media/{media_id}/original", ID: "headOriginal", Streaming: true},
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
func New(db *sql.DB, assets fs.FS, version string, service *auth.Service, origin string, mediaService *media.Service, jobService *jobs.Admin, sourceService *library.Sources, scanService *scanner.Scans, readiness *health.Service, batches *access.AlbumBatches) http.Handler {
	mux := http.NewServeMux()
	authentication := authentication{service: service, origin: origin}
	authorization := authorization{service: access.New(db, service)}
	content := mediaHandler{service: mediaService}
	work := jobHandler{service: jobService}
	sources := sourceHandler{service: sourceService}
	scans := scanHandler{service: scanService}
	tagBatches := albumBatchHandler{service: batches}
	handlers := map[string]http.HandlerFunc{
		"login":                    authentication.login,
		"logout":                   authentication.logout,
		"me":                       authentication.me,
		"getCsrf":                  authentication.csrf,
		"changePassword":           authentication.password,
		"listSessions":             authentication.sessions,
		"revokeSession":            authentication.revoke,
		"listUsers":                authentication.users,
		"createUser":               authentication.createUser,
		"getUser":                  authentication.user,
		"updateUser":               authentication.updateUser,
		"deleteUser":               authentication.deleteUser,
		"resetUserPassword":        authentication.resetPassword,
		"listUserSessions":         authentication.userSessions,
		"revokeUserSession":        authentication.revokeUserSession,
		"listAudit":                authentication.audit,
		"listExactDuplicateGroups": content.exactDuplicates,
		"listExactDuplicateMedia":  content.exactDuplicateMedia,
		"listSourceRoots":          sources.roots,
		"listSources":              sources.list,
		"createSource":             sources.create,
		"getSource":                sources.get,
		"updateSource":             sources.update,
		"deleteSource":             sources.remove,
		"checkSource":              sources.check,
		"enqueueScan":              scans.enqueue,
		"enqueueTagBatch":          tagBatches.enqueue,
		"enqueueCoverBatch":        tagBatches.enqueueCovers,
		"listCoverBatchTargets":    tagBatches.coverTargets,
		"listTagBatchTargets":      tagBatches.targets,
		"listTagBatchResults":      tagBatches.results,
		"listJobs":                 work.list,
		"getJob":                   work.get,
		"listJobAttempts":          work.attempts,
		"listJobIssues":            work.issues,
		"cancelJob":                work.cancel,
		"retryJob":                 work.retry,
		"listTags":                 authorization.tags,
		"createTag":                authorization.createTag,
		"getTag":                   authorization.tag,
		"renameTag":                authorization.renameTag,
		"deleteTag":                authorization.deleteTag,
		"listGroups":               authorization.groups,
		"createGroup":              authorization.createGroup,
		"getGroup":                 authorization.group,
		"renameGroup":              authorization.renameGroup,
		"deleteGroup":              authorization.deleteGroup,
		"getUserGroups":            authorization.userGroups,
		"setUserGroups":            authorization.setUserGroups,
		"listAdminLibraries":       authorization.adminLibraries,
		"createLibrary":            authorization.createLibrary,
		"getAdminLibrary":          authorization.adminLibrary,
		"updateLibrary":            authorization.updateLibrary,
		"deleteLibrary":            authorization.deleteLibrary,
		"getLibraryPolicy":         authorization.libraryPolicy,
		"setLibraryPolicy":         authorization.setLibraryPolicy,
		"listAlbumTags":            authorization.albumTags,
		"getAlbumCreator":          authorization.albumCreator,
		"setAlbumCreator":          authorization.setAlbumCreator,
		"getAdminAlbumTags":        authorization.adminAlbumTags,
		"setAlbumTags":             authorization.setAlbumTags,
		"getAlbumPolicy":           authorization.albumPolicy,
		"getAdminAlbumMetadata":    authorization.albumMetadata,
		"updateAlbumMetadata":      authorization.updateAlbumMetadata,
		"setAlbumPolicy":           authorization.setAlbumPolicy,
		"listLibraries":            authorization.libraries,
		"getLibrary":               authorization.library,
		"listAlbums":               authorization.albums,
		"getAdminPersonMetadata":   authorization.personMetadata,
		"updatePersonMetadata":     authorization.updatePersonMetadata,
		"listPeople":               authorization.people,
		"getPerson":                authorization.person,
		"listPersonAlbums":         authorization.personAlbums,
		"getAlbum":                 authorization.album,
		"listFavoriteCollections":  authorization.favoriteCollections,
		"createFavoriteCollection": authorization.createFavoriteCollection,
		"getFavoriteCollection":    authorization.favoriteCollection,
		"renameFavoriteCollection": authorization.renameFavoriteCollection,
		"getAlbumFavorite":         authorization.albumFavorite,
		"setAlbumFavorite":         authorization.setAlbumFavorite,
		"deleteAlbumFavorite":      authorization.setAlbumFavorite,
		"getAlbumRating":           authorization.albumRating,
		"setAlbumRating":           authorization.setAlbumRating,
		"deleteAlbumRating":        authorization.setAlbumRating,
		"getAlbumView":             authorization.albumView,
		"recordAlbumView":          authorization.recordAlbumView,
		"listAlbumMedia":           content.albumMedia,
		"getMedia":                 content.get,
		"listVariants":             content.variants,
		"getVariantContent":        content.variantContent,
		"headVariantContent":       content.variantContent,
		"getOriginal":              content.original,
		"headOriginal":             content.original,
		"live":                     func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, Health{"ok"}) },
		"ready": func(w http.ResponseWriter, r *http.Request) {
			state, err := readiness.Check(r.Context())
			if err != nil {
				slog.Warn("readiness dependency unavailable", "request_id", w.Header().Get("X-Request-ID"), "error", err)
				problem(w, http.StatusServiceUnavailable, "NOT_READY", "A required service dependency is unavailable.")
				return
			}
			writeJSON(w, http.StatusOK, state)
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
		if !op.Streaming {
			handler = metadataTimeout(handler)
		}
		mux.HandleFunc(op.Method+" /api/v2"+op.Path, handler)
		if !slices.Contains(allowed[op.Path], op.Method) {
			allowed[op.Path] = append(allowed[op.Path], op.Method)
		}
		if op.Method == "GET" && !slices.Contains(allowed[op.Path], "HEAD") {
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
		// Reject ambiguous paths before ServeMux can redirect a private-looking
		// traversal or duplicate slash into the SPA or a different API route.
		if strings.Contains(r.URL.Path, "//") || strings.ContainsRune(r.URL.Path, 0) {
			problem(w, 404, "NOT_FOUND", "The resource does not exist.")
			return
		}
		for _, segment := range strings.Split(r.URL.Path, "/") {
			if segment == "." || segment == ".." {
				problem(w, 404, "NOT_FOUND", "The resource does not exist.")
				return
			}
		}
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
	for _, prefix := range []string{"data", "cache", "sources", "media", "variants", "originals", "static"} {
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			problem(w, 404, "NOT_FOUND", "The private resource does not exist.")
			return
		}
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
