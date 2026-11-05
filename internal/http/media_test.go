package httpapi

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"image"
	"image/png"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

type protectedFixture struct {
	db          *sql.DB
	http        http.Handler
	service     *media.Service
	admin       adminClient
	reader      adminClient
	user        auth.User
	data        []byte
	hash        string
	source      string
	original    string
	cache       string
	variant     string
	variantURL  string
	originalURL string
}

func protectedMedia(t *testing.T) protectedFixture {
	t.Helper()
	db, oldHandler := authFixture(t)
	admin, _ := ownerClient(t, oldHandler)
	user := responseUser(t, admin.request("POST", "/api/v2/admin/users", map[string]any{"username": "media-reader", "password": testPassword, "download_original": true}, "", store.NewID()), 201)
	dir := t.TempDir()
	cache, source := filepath.Join(dir, "cache"), filepath.Join(dir, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".marker"), []byte("test mount"), 0o600); err != nil {
		t.Fatal(err)
	}
	picture := image.NewNRGBA(image.Rect(0, 0, 256, 128))
	if _, err := rand.Read(picture.Pix); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	original := filepath.Join(source, "portrait.png")
	if err := os.WriteFile(original, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(original)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := media.VariantPath("revision", "fixture_png", 1, hash, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	variant := filepath.Join(cache, filepath.FromSlash(storage))
	if err := os.MkdirAll(filepath.Dir(variant), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(variant, data, 0o600); err != nil {
		t.Fatal(err)
	}
	queries := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO permission_groups(id,name,created_at) VALUES('media-group','Media fixture',0)`, nil},
		{`INSERT INTO libraries(id,name,created_at) VALUES('media-library','Media fixture',0)`, nil},
		{`INSERT INTO sources(id,library_id,name,root_path,root_config_sha256,mapping_json,state,created_at) VALUES('media-source','media-library','fixture',?,?,'{}','healthy',0)`, []any{source, (media.SourceRoot{Path: source, Marker: ".marker"}).Signature()}},
		{`INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES('media-album','media-library','media-source','Portrait',0,0)`, nil},
		{`INSERT INTO user_groups(user_id,group_id) VALUES(?,'media-group')`, []any{user.ID}},
		{`INSERT INTO library_access(library_id,group_id) VALUES('media-library','media-group')`, nil},
		{`INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,sha256,source_config_version,updated_at) VALUES('media-entry','media-source','portrait.png',?,?,?,1,0)`, []any{len(data), info.ModTime().UnixNano(), hash}},
		{`INSERT INTO media(id,album_id,source_entry_id,kind,file_name,mime,width,height,current_revision_id,created_at,updated_at) VALUES('protected','media-album','media-entry','image','Portrait 中文.png','image/png',256,128,'revision',0,0)`, nil},
		{`INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) VALUES('revision','protected',?,?,?,1,0)`, []any{hash, len(data), info.ModTime().UnixNano()}},
		{`INSERT INTO media_variants(id,revision_id,preset_key,preset_version,storage_path,mime,size_bytes,sha256,width,height,status,created_at) VALUES('protected-variant','revision','fixture_png',1,?,'image/png',?,?,256,128,'ready',0)`, []any{storage, len(data), hash}},
	}
	for _, query := range queries {
		if _, err := db.Exec(query.sql, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	a, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	service := media.New(db, a, cache, map[string]media.SourceRoot{"fixture": {Path: source, Marker: ".marker"}})
	h := New(db, fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>GalleryGo</title>")}}, "media-test", a, testOrigin, service, testJobAdmin(t, db, a), testSourceAdmin(t, db, a, map[string]media.SourceRoot{"fixture": {Path: source, Marker: ".marker"}}), testScans(t, db, a), testReadiness(t, db, a, map[string]media.SourceRoot{"fixture": {Path: source, Marker: ".marker"}}))
	admin.handler = h
	reader := accountClient(t, h, user.Username, testPassword)
	return protectedFixture{db, h, service, admin, reader, user, data, hash, source, original, cache, variant, "/api/v2/media/protected/variants/protected-variant/content", "/api/v2/media/protected/original"}
}

func mediaRequest(client adminClient, method, path string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	for key, values := range headers {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	if client.cookie != nil {
		r.AddCookie(client.cookie)
	}
	w := httptest.NewRecorder()
	client.handler.ServeHTTP(w, r)
	return w
}

func assertPrivateContent(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Vary") != "Cookie" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("unsafe media headers: %v", w.Header())
	}
}

func TestMediaHTTPGetHeadAndSingleRanges(t *testing.T) {
	f := protectedMedia(t)
	for _, path := range []string{f.variantURL, f.originalURL} {
		w := mediaRequest(f.reader, "GET", path, nil)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), f.data) || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Content-Length") != strconv.Itoa(len(f.data)) {
			t.Fatalf("full content: %d %v", w.Code, w.Header())
		}
		assertPrivateContent(t, w)
		kind, parameters, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
		if err != nil || parameters["filename"] != "Portrait 中文.png" || (path == f.originalURL && kind != "attachment") || (path == f.variantURL && kind != "inline") {
			t.Fatal("unsafe or incorrect disposition", w.Header(), err)
		}
		etag := w.Header().Get("ETag")
		if etag != `"sha256-`+f.hash+`"` || w.Header().Get("Last-Modified") != "" {
			t.Fatal("incorrect content validator")
		}
		for _, headers := range []http.Header{nil, {"Range": {"bytes=0-1"}}, {"Range": {"bytes=0-1,4-5"}}} {
			w = mediaRequest(f.reader, "HEAD", path, headers)
			if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(len(f.data)) {
				t.Fatalf("HEAD: %d %v %d", w.Code, w.Header(), w.Body.Len())
			}
			assertPrivateContent(t, w)
		}
		for _, test := range []struct {
			header     string
			start, end int
		}{
			{"bytes=0-0", 0, 1}, {"bytes=1-3", 1, 4}, {"bytes=4-", 4, len(f.data)}, {"bytes=-4", len(f.data) - 4, len(f.data)},
			{"bytes=0-999999", 0, len(f.data)}, {"bytes=-999999", 0, len(f.data)},
		} {
			w = mediaRequest(f.reader, "GET", path, http.Header{"Range": {test.header}})
			if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), f.data[test.start:test.end]) || w.Header().Get("Content-Length") != strconv.Itoa(test.end-test.start) {
				t.Fatalf("range %s: %d %v", test.header, w.Code, w.Header())
			}
			want := "bytes " + strconv.Itoa(test.start) + "-" + strconv.Itoa(test.end-1) + "/" + strconv.Itoa(len(f.data))
			if w.Header().Get("Content-Range") != want {
				t.Fatal("incorrect Content-Range", w.Header())
			}
			assertPrivateContent(t, w)
		}
	}
}

func TestMediaHTTPConditionalOrderingAndProblemRanges(t *testing.T) {
	f := protectedMedia(t)
	etag := `"sha256-` + f.hash + `"`
	for _, path := range []string{f.variantURL, f.originalURL} {
		for _, bad := range []string{"", "bytes=", "bytes=-0", "bytes=-000", "bytes=0-1,4-5", "bytes=2-1", "bytes=-", "bytes=+1-2", "bytes=0-+2", "bytes=0- 2", "bytes=999999-", "bytes=9223372036854775808-", "items=0-1"} {
			w := mediaRequest(f.reader, "GET", path, http.Header{"Range": {bad}})
			expectCode(t, w, 416, "INVALID_RANGE")
			if w.Header().Get("Content-Range") != "bytes */"+strconv.Itoa(len(f.data)) || w.Header().Get("ETag") != etag || w.Header().Get("Content-Disposition") != "" {
				t.Fatal("range error lost privacy/size/validator boundary", w.Header())
			}
			assertPrivateContent(t, w)
			if strings.Contains(w.Body.String(), f.source) || strings.Contains(w.Body.String(), f.cache) {
				t.Fatal("range error leaked private paths")
			}
		}
		expectCode(t, mediaRequest(f.reader, "GET", path, http.Header{"Range": {"bytes=0-1", "bytes=4-5"}}), 416, "INVALID_RANGE")
		for _, validator := range []string{`"old"`, "W/" + etag, "Mon, 02 Jan 2006 15:04:05 GMT"} {
			w := mediaRequest(f.reader, "GET", path, http.Header{"Range": {"bytes=0-1"}, "If-Range": {validator}})
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), f.data) {
				t.Fatalf("stale/weak/date If-Range should ignore Range: %s %d", validator, w.Code)
			}
		}
		w := mediaRequest(f.reader, "GET", path, http.Header{"Range": {"bytes=0-1"}, "If-Range": {etag}})
		if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), f.data[:2]) {
			t.Fatal("matching If-Range did not resume")
		}
		w = mediaRequest(f.reader, "GET", path, http.Header{"Range": {"bytes=0-1,4-5"}, "If-Range": {`"old"`}})
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), f.data) {
			t.Fatal("rejected multipart before If-Range could ignore Range")
		}
		for _, method := range []string{"GET", "HEAD"} {
			w = mediaRequest(f.reader, method, path, http.Header{"If-None-Match": {etag}, "Range": {"bytes=-0"}})
			if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("conditional ordering: %s %d %v", method, w.Code, w.Header())
			}
		}
		expectCode(t, mediaRequest(f.reader, "GET", path, http.Header{"If-Match": {`"old"`}}), 412, "PRECONDITION_FAILED")
	}
}

func TestMediaHTTPAuthorizationRunsBeforeHeadRangeOrConditionalResponses(t *testing.T) {
	f := protectedMedia(t)
	etag := `"sha256-` + f.hash + `"`
	for _, path := range []string{f.variantURL, f.originalURL} {
		for _, method := range []string{"GET", "HEAD"} {
			w := mediaRequest(adminClient{handler: f.http}, method, path, http.Header{"Range": {"bytes=0-0"}, "If-None-Match": {etag}})
			expectCode(t, w, 401, "UNAUTHENTICATED")
			if w.Header().Get("ETag") != "" || w.Header().Get("Content-Range") != "" {
				t.Fatal("anonymous request received media metadata")
			}
		}
	}
	if _, err := f.db.Exec(`UPDATE users SET download_original=0 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "HEAD"} {
		for _, headers := range []http.Header{nil, {"Range": {"bytes=0-1"}}, {"If-None-Match": {etag}}} {
			expectCode(t, mediaRequest(f.reader, method, f.originalURL, headers), 404, "NOT_FOUND")
		}
	}
	if _, err := f.db.Exec(`UPDATE users SET download_original=0 WHERE role='admin'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, mediaRequest(f.admin, "GET", f.originalURL, nil), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`DELETE FROM library_access`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.variantURL, f.originalURL, "/api/v2/media/protected", "/api/v2/media/protected/variants", "/api/v2/albums/media-album/media"} {
		expectCode(t, mediaRequest(f.reader, "GET", path, http.Header{"If-None-Match": {etag}, "Range": {"bytes=0-1"}}), 404, "NOT_FOUND")
	}
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, mediaRequest(f.reader, "HEAD", f.variantURL, nil), 401, "UNAUTHENTICATED")
}

func TestMediaHTTPMetadataReadyOwnershipAndSourceOutage(t *testing.T) {
	f := protectedMedia(t)
	response := mediaRequest(f.reader, "GET", "/api/v2/media/protected", nil)
	item := decodeResponse[media.Metadata](t, response, 200)
	if item.ID != "protected" || item.FileName != "Portrait 中文.png" || item.OfflineCapable || item.Revision == nil || item.Orientation != nil || item.CaptureAt != nil || !strings.Contains(response.Body.String(), `"orientation":null`) || !strings.Contains(response.Body.String(), `"capture_at":null`) {
		t.Fatal("incorrect metadata or absent explicit unknown fields", item, response.Body.String())
	}
	captured := time.Date(2026, 11, 5, 4, 34, 56, 0, time.UTC)
	if _, err := f.db.Exec(`UPDATE media SET orientation=6,capture_at=? WHERE id='protected'`, captured.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	response = mediaRequest(f.reader, "GET", "/api/v2/media/protected", nil)
	item = decodeResponse[media.Metadata](t, response, 200)
	if item.Orientation == nil || *item.Orientation != 6 || item.CaptureAt == nil || !item.CaptureAt.Equal(captured) || !strings.Contains(response.Body.String(), `"capture_at":"2026-11-05T04:34:56Z"`) || strings.Contains(response.Body.String(), f.source) || strings.Contains(response.Body.String(), "GPS") {
		t.Fatal("whitelisted HTTP metadata lost UTC/privacy boundary", item, response.Body.String())
	}
	page := decodeResponse[listPage[media.Metadata]](t, mediaRequest(f.reader, "GET", "/api/v2/albums/media-album/media", nil), 200)
	if len(page.Items) != 1 || page.Items[0].ID != item.ID || page.Items[0].Orientation == nil || *page.Items[0].Orientation != 6 || page.Items[0].CaptureAt == nil || !page.Items[0].CaptureAt.Equal(captured) {
		t.Fatal("album/media visibility or metadata mismatch")
	}
	variants := decodeResponse[listPage[media.Variant]](t, mediaRequest(f.reader, "GET", "/api/v2/media/protected/variants", nil), 200)
	if len(variants.Items) != 1 || variants.Items[0].ContentURL != f.variantURL || variants.Items[0].SHA256 != f.hash {
		t.Fatal("incorrect variant metadata")
	}
	expectCode(t, mediaRequest(f.reader, "GET", "/api/v2/media/other/variants/protected-variant/content", nil), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`UPDATE media_variants SET status='retired'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, mediaRequest(f.reader, "GET", f.variantURL, nil), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`UPDATE media_variants SET status='ready'; UPDATE sources SET state='unavailable'`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, mediaRequest(f.reader, "GET", f.originalURL, nil), 503, "SOURCE_UNAVAILABLE")
	if w := mediaRequest(f.reader, "GET", f.variantURL, nil); w.Code != 200 {
		t.Fatal("source outage removed readable private cache", w.Body.String())
	}
	if err := os.Remove(f.variant); err != nil {
		t.Fatal(err)
	}
	w := mediaRequest(f.reader, "GET", f.variantURL, nil)
	expectCode(t, w, 503, "VARIANT_UNAVAILABLE")
	if strings.Contains(w.Body.String(), f.cache) || strings.Contains(w.Body.String(), f.source) {
		t.Fatal("unavailable error leaked a path")
	}
	for _, path := range []string{"/cache/variants/revision", "/sources/portrait", "/static/original", "/media/raw"} {
		expectCode(t, mediaRequest(f.reader, "GET", path, nil), 404, "NOT_FOUND")
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines  []time.Time
	writeHook  func()
	writeError error
}

func (w *deadlineRecorder) SetWriteDeadline(value time.Time) error {
	w.deadlines = append(w.deadlines, value)
	return nil
}
func (w *deadlineRecorder) Write(data []byte) (int, error) {
	if w.writeError != nil {
		return 0, w.writeError
	}
	n, err := w.ResponseRecorder.Write(data)
	if w.writeHook != nil {
		hook := w.writeHook
		w.writeHook = nil
		hook()
	}
	return n, err
}

func TestMediaHTTPIdleDeadlineAndInterruptedStreamAreNotSilentSuccess(t *testing.T) {
	f := protectedMedia(t)
	before := time.Now()
	readerAuth, err := f.serviceAuth(t)
	if err != nil {
		t.Fatal(err)
	}
	content, err := f.service.OpenOriginal(t.Context(), readerAuth, "protected")
	if err != nil {
		t.Fatal(err)
	}
	writer := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	serveMedia(writer, httptest.NewRequest("GET", f.originalURL, nil), content)
	content.Close()
	if len(writer.deadlines) < 3 || !writer.deadlines[len(writer.deadlines)-1].IsZero() {
		t.Fatal("write deadlines were not refreshed/reset", writer.deadlines)
	}
	if writer.deadlines[0].Before(before.Add(29 * time.Second)) {
		t.Fatal("stream inherited a short metadata deadline")
	}
	content, err = f.service.OpenOriginal(t.Context(), readerAuth, "protected")
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	writer = &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), writeHook: func() {
		if err := os.WriteFile(f.original, bytes.Repeat([]byte{'x'}, len(f.data)), 0o600); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(time.Second)
		if err := os.Chtimes(f.original, future, future); err != nil {
			t.Fatal(err)
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
		serveMedia(writer, httptest.NewRequest("GET", f.originalURL, nil), content)
	}()
	if !aborted || writer.Body.Len() >= len(f.data) || !bytes.Equal(writer.Body.Bytes(), f.data[:writer.Body.Len()]) {
		t.Fatal("changed source was returned as a complete or mixed response")
	}
}

func (f protectedFixture) serviceAuth(t *testing.T) (auth.Session, error) {
	t.Helper()
	a, err := auth.New(f.db, auth.DefaultPolicy())
	if err != nil {
		return auth.Session{}, err
	}
	return a.Authenticate(t.Context(), f.reader.cookie.Value)
}

func TestMediaHTTPBrokenWriteAbortsWithoutAppendingJSON(t *testing.T) {
	f := protectedMedia(t)
	session, err := f.serviceAuth(t)
	if err != nil {
		t.Fatal(err)
	}
	content, err := f.service.OpenVariant(t.Context(), session, "protected", "protected-variant")
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	writer := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder(), writeError: os.ErrDeadlineExceeded}
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
		serveMedia(writer, httptest.NewRequest("GET", f.variantURL, nil), content)
	}()
	if !aborted || writer.Body.Len() != 0 {
		t.Fatal("a failed write did not abort or appended an error body to media")
	}
}

func TestContentOperationsDoNotUseMetadataTimeoutOrPublicAccess(t *testing.T) {
	count := 0
	for _, operation := range Operations {
		isContent := strings.HasSuffix(operation.Path, "/content") || strings.HasSuffix(operation.Path, "/original")
		if operation.Streaming != isContent || isContent && operation.Public {
			t.Fatalf("incorrect stream boundary: %+v", operation)
		}
		if isContent {
			count++
		}
	}
	if count != 4 {
		t.Fatalf("streaming operation count=%d", count)
	}
}
