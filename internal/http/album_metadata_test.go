package httpapi

import (
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func TestAlbumMetadataHTTPVersionsReceiptsClearAndPublicProjection(t *testing.T) {
	f := protectedMedia(t)
	if _, err := f.db.Exec(`INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('selected-cover','media-album','image','selected.png',0,0)`); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v2/admin/albums/media-album"
	initial := f.admin.request("GET", path, nil, "", "")
	before := decodeResponse[access.AlbumMetadata](t, initial, 200)
	if initial.Header().Get("ETag") != `"album:media-album:1"` || before.SourceName != "Portrait" || before.DisplayName != nil || before.CoverOverrideID != nil || initial.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("admin metadata did not expose bounded versioned preferences", before, initial.Header())
	}
	key := store.NewID()
	patch := map[string]any{"display_name": "  Edited title  ", "cover_media_id": "selected-cover"}
	changedResponse := f.admin.request("PATCH", path, patch, initial.Header().Get("ETag"), key)
	changed := decodeResponse[access.AlbumMetadata](t, changedResponse, 200)
	if changed.Version != 2 || changed.DisplayName == nil || *changed.DisplayName != "Edited title" || changed.CoverMediaID == nil || *changed.CoverMediaID != "selected-cover" || changed.CoverOverrideID == nil || *changed.CoverOverrideID != "selected-cover" || changedResponse.Header().Get("ETag") != `"album:media-album:2"` {
		t.Fatal("PATCH did not publish its exact metadata version/cover", changed, changedResponse.Header())
	}
	public := decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	if public.Title != "Edited title" || public.CoverMediaID == nil || *public.CoverMediaID != "selected-cover" || public.Version != 2 {
		t.Fatal("public album did not project the metadata override", public)
	}
	replayed := f.admin.request("PATCH", path, patch, initial.Header().Get("ETag"), key)
	if value := decodeResponse[access.AlbumMetadata](t, replayed, 200); !reflect.DeepEqual(value, changed) || replayed.Header().Get("ETag") != changedResponse.Header().Get("ETag") {
		t.Fatal("HTTP retry did not return historical actor-bound result", value, changed)
	}
	noop := decodeResponse[access.AlbumMetadata](t, f.admin.request("PATCH", path, patch, changedResponse.Header().Get("ETag"), store.NewID()), 200)
	if !reflect.DeepEqual(noop, changed) {
		t.Fatal("same override request changed version", noop, changed)
	}
	clearResponse := f.admin.request("PATCH", path, map[string]any{"display_name": "", "cover_media_id": ""}, changedResponse.Header().Get("ETag"), store.NewID())
	cleared := decodeResponse[access.AlbumMetadata](t, clearResponse, 200)
	if cleared.Version != 3 || cleared.DisplayName != nil || cleared.CoverOverrideID != nil || cleared.CoverMediaID == nil || *cleared.CoverMediaID != "selected-cover" {
		t.Fatal("explicit empty strings did not clear desired preferences/sticky cover", cleared)
	}
	public = decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	if public.Title != "Portrait" {
		t.Fatal("cleared display did not restore indexed label", public)
	}
	decodeResponse[access.AlbumMetadata](t, f.admin.request("PATCH", path, patch, initial.Header().Get("ETag"), key), 200)
	expectCode(t, f.admin.request("PATCH", path, patch, initial.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	expectCode(t, f.admin.request("PATCH", path, map[string]any{"display_name": "Different"}, initial.Header().Get("ETag"), key), 409, "IDEMPOTENCY_CONFLICT")
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	unmounted := decodeResponse[access.AlbumMetadata](t, f.admin.request("PATCH", path, map[string]any{"display_name": "Offline source label"}, clearResponse.Header().Get("ETag"), store.NewID()), 200)
	if unmounted.Version != 4 || unmounted.SourceName != "Portrait" {
		t.Fatal("metadata mutation probed or reassigned the missing source", unmounted)
	}
	for _, secret := range []string{f.source, "root_path", "relative_path", "marker", "source_config"} {
		if strings.Contains(changedResponse.Body.String(), secret) {
			t.Fatal("metadata result exposed private filesystem/pin context", secret)
		}
	}
}

func TestAlbumMetadataHTTPRejectsUnsafeBodiesHeadersRolesAndJournalFailures(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/admin/albums/media-album"
	tag := `"album:media-album:1"`
	for _, client := range []struct {
		value  adminClient
		status int
		code   string
	}{{adminClient{handler: f.http}, 401, "UNAUTHENTICATED"}, {f.reader, 403, "ADMIN_REQUIRED"}, {adminClient{handler: f.http, cookie: &http.Cookie{Name: sessionCookie, Value: strings.Repeat("x", 43)}}, 401, "UNAUTHENTICATED"}} {
		for _, route := range []string{path, "/api/v2/admin/albums/missing"} {
			expectCode(t, client.value.request("GET", route, nil, "", ""), client.status, client.code)
			expectCode(t, client.value.request("PATCH", route, map[string]any{"display_name": "Denied"}, tag, store.NewID()), client.status, client.code)
		}
	}
	expectCode(t, f.admin.request("PATCH", path, map[string]any{"display_name": "Name"}, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
	for _, invalid := range []string{"*", `W/"album:media-album:1"`, `"album:other:1"`, `"album:media-album:0"`} {
		expectCode(t, f.admin.request("PATCH", path, map[string]any{"display_name": "Name"}, invalid, store.NewID()), 400, "INVALID_PRECONDITION")
	}
	missingKey := f.admin.request("PATCH", path, map[string]any{"display_name": "Name"}, tag, "")
	expectCode(t, missingKey, 422, "VALIDATION_FAILED")
	for _, body := range []map[string]any{{"display_name": nil}, {"cover_media_id": nil}, {"source_name": "rewrite source"}, {"library_id": "other"}, {"person_id": "other"}, {"restricted": false}, {"Display_Name": "ambiguous"}, {"display_name": true}} {
		expectCode(t, f.admin.request("PATCH", path, body, tag, store.NewID()), 400, "INVALID_JSON")
	}
	for _, body := range []map[string]any{{}, {"display_name": "   "}, {"cover_media_id": "foreign"}} {
		expectCode(t, f.admin.request("PATCH", path, body, tag, store.NewID()), 422, "VALIDATION_FAILED")
	}
	deniedCSRF := requestAuth(f.http, "PATCH", path, `{"display_name":"Denied"}`, testOrigin, "", f.admin.cookie)
	expectCode(t, deniedCSRF, 403, "CSRF_REJECTED")
	expectCode(t, f.admin.request("GET", "/api/v2/admin/albums/missing", nil, "", ""), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`CREATE TRIGGER reject_album_metadata_receipt BEFORE INSERT ON mutation_receipts BEGIN SELECT RAISE(ABORT,'/private/SQL/receipt'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("PATCH", path, map[string]any{"display_name": "Must roll back"}, tag, store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(problemContent(t, failed), "/private") || strings.Contains(problemContent(t, failed), "receipt") || strings.Contains(problemContent(t, failed), "SQL") {
		t.Fatal("late mutation error exposed internal details", failed.Body.String())
	}
	value := decodeResponse[access.AlbumMetadata](t, f.admin.request("GET", path, nil, "", ""), 200)
	if value.Version != 1 || value.DisplayName != nil {
		t.Fatal("failed receipt left a partial metadata version", value)
	}
}
