package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestOriginalDownloadExpectedUserFence(t *testing.T) {
	f := protectedMedia(t)
	var changesBefore, outboxBefore, auditBefore int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`).Scan(&changesBefore, &outboxBefore, &auditBefore); err != nil {
		t.Fatal(err)
	}
	matching := f.originalURL + "?expected_user_id=" + url.QueryEscape(f.user.ID) + "&expected_revision_id=revision"
	wrong := f.originalURL + "?expected_user_id=another-account&expected_revision_id=revision"
	wrongRevision := f.originalURL + "?expected_user_id=" + url.QueryEscape(f.user.ID) + "&expected_revision_id=old-revision"

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		w := mediaRequest(f.reader, method, matching, nil)
		if w.Code != http.StatusOK || (method == http.MethodGet && !bytes.Equal(w.Body.Bytes(), f.data)) {
			t.Fatalf("matching actor %s: status=%d", method, w.Code)
		}
		for _, path := range []string{wrong, wrongRevision} {
			for _, headers := range []http.Header{
				nil,
				{"Range": {"bytes=0-1"}},
				{"If-None-Match": {`"sha256-` + f.hash + `"`}},
			} {
				w = mediaRequest(f.reader, method, path, headers)
				if w.Code != http.StatusNotFound {
					t.Fatalf("mismatched account or revision %s returned status %d, want 404", method, w.Code)
				}
				expectCode(t, w, http.StatusNotFound, "NOT_FOUND")
				if w.Header().Get("ETag") != "" || w.Header().Get("Content-Range") != "" {
					t.Fatal("mismatched account or revision received content metadata")
				}
			}
		}
	}
	w := mediaRequest(f.reader, http.MethodGet, matching, http.Header{"Range": {"bytes=0-1"}})
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), f.data[:2]) {
		t.Fatal("matching account lost existing Range support")
	}
	w = mediaRequest(f.reader, http.MethodHead, matching, http.Header{"If-None-Match": {`"sha256-` + f.hash + `"`}})
	if w.Code != http.StatusNotModified {
		t.Fatal("matching account lost conditional HEAD support")
	}
	marker := filepath.Join(f.source, ".marker")
	heldMarker := filepath.Join(f.source, ".marker-held")
	if err := os.Rename(marker, heldMarker); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(heldMarker, marker) })
	for _, path := range []string{wrong, wrongRevision} {
		expectCode(t, mediaRequest(f.reader, http.MethodGet, path, nil), http.StatusNotFound, "NOT_FOUND")
	}
	expectCode(t, mediaRequest(f.reader, http.MethodGet, matching, nil), http.StatusServiceUnavailable, "SOURCE_UNAVAILABLE")
	if err := os.Rename(heldMarker, marker); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"expected_user_id=",
		"expected_user_id=" + url.QueryEscape(f.user.ID) + "&expected_user_id=" + url.QueryEscape(f.user.ID),
		"expected_user_id=%GG",
		"expected_user_id=%FF",
		"expected_user_id=%00",
		"expected_revision_id=",
		"expected_revision_id=revision&expected_revision_id=revision",
		"expected_revision_id=%FF",
		"expected_revision_id=%00",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			r := httptest.NewRequest(method, f.originalURL, nil)
			r.URL.RawQuery = query
			r.AddCookie(f.reader.cookie)
			w = httptest.NewRecorder()
			f.http.ServeHTTP(w, r)
			expectCode(t, w, http.StatusBadRequest, "INVALID_FILTER")
		}
	}
	expectCode(t, mediaRequest(adminClient{handler: f.http}, http.MethodGet, matching, nil), http.StatusUnauthorized, "UNAUTHENTICATED")
	if _, err := f.db.Exec(`UPDATE users SET download_original=0 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, mediaRequest(f.reader, http.MethodGet, matching, nil), http.StatusNotFound, "NOT_FOUND")
	var changesAfter, outboxAfter, auditAfter int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`).Scan(&changesAfter, &outboxAfter, &auditAfter); err != nil {
		t.Fatal(err)
	}
	if changesAfter != changesBefore || outboxAfter != outboxBefore || auditAfter != auditBefore {
		t.Fatal("GET or HEAD created a download event")
	}
}

func TestOriginalDownloadRejectsStaleRevision(t *testing.T) {
	f := protectedMedia(t)
	oldURL := f.originalURL + "?expected_user_id=" + url.QueryEscape(f.user.ID) + "&expected_revision_id=revision"
	if _, err := f.db.Exec(`INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) SELECT 'revision-new',media_id,source_sha256,source_size,source_mtime_ns,version+1,created_at FROM media_revisions WHERE id='revision'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE media SET current_revision_id='revision-new',version=version+1 WHERE id='protected'`); err != nil {
		t.Fatal(err)
	}
	w := mediaRequest(f.reader, http.MethodGet, oldURL, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("old revision returned status %d, want 404", w.Code)
	}
	expectCode(t, w, http.StatusNotFound, "NOT_FOUND")
	currentURL := f.originalURL + "?expected_user_id=" + url.QueryEscape(f.user.ID) + "&expected_revision_id=revision-new"
	w = mediaRequest(f.reader, http.MethodGet, currentURL, nil)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), f.data) {
		t.Fatalf("current revision returned %d", w.Code)
	}
}
