package httpapi

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func TestAlbumViewsHTTPFreshAuthorityPersonalValidatorsAndReplay(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/albums/media-album/views"
	initial := f.reader.request("GET", path, nil, "", "")
	before := decodeResponse[access.AlbumView](t, initial, 200)
	adminInitial := f.admin.request("GET", path, nil, "", "")
	decodeResponse[access.AlbumView](t, adminInitial, 200)
	if before.ViewCount != 0 || before.Version != 1 || before.LastViewedAt != nil || initial.Header().Get("ETag") == "" || initial.Header().Get("ETag") == adminInitial.Header().Get("ETag") || initial.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("initial view state was invented, shared or cacheable", before, initial.Header())
	}
	key := store.NewID()
	response := f.reader.request("POST", path, nil, "", key)
	one := decodeResponse[access.AlbumView](t, response, 200)
	if one.ViewCount != 1 || one.Version != 2 || one.LastViewedAt == nil || response.Header().Get("ETag") == initial.Header().Get("ETag") {
		t.Fatal("view did not advance the current account's personal revision", one, response.Header())
	}
	admin := decodeResponse[access.AlbumView](t, f.admin.request("POST", path, nil, "", key), 200)
	if admin.ViewCount != 1 || admin.Version != 2 {
		t.Fatal("same key for a different actor did not record an independent view", admin)
	}
	if current := f.reader.request("GET", path, nil, "", ""); !reflect.DeepEqual(decodeResponse[access.AlbumView](t, current, 200), one) || current.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("another actor changed the personal view validator", current)
	}
	shared := decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	if shared.ViewCount != 2 || shared.Version != 3 {
		t.Fatal("album did not expose the current shared view total", shared)
	}
	two := decodeResponse[access.AlbumView](t, f.reader.request("POST", path, nil, "", store.NewID()), 200)
	if two.ViewCount != 2 || two.Version != 3 {
		t.Fatal("new idempotency key failed to record a distinct view", two)
	}
	if replay := decodeResponse[access.AlbumView](t, f.reader.request("POST", path, nil, "", key), 200); !reflect.DeepEqual(replay, one) {
		t.Fatal("historical view replay changed its personal result", replay, one)
	}
	if current := decodeResponse[access.AlbumView](t, f.reader.request("GET", path, nil, "", ""), 200); !reflect.DeepEqual(current, two) {
		t.Fatal("historical replay counted a second view", current, two)
	}
	guest := adminClient{handler: f.http}
	for _, method := range []string{"GET", "POST"} {
		expectCode(t, guest.request(method, path, nil, "", store.NewID()), 401, "UNAUTHENTICATED")
		expectCode(t, f.reader.request(method, path+"?user_id=other", nil, "", store.NewID()), 400, "INVALID_FILTER")
	}
	expectCode(t, f.reader.request("POST", path, nil, "", ""), 422, "VALIDATION_FAILED")
	expectCode(t, f.reader.request("POST", path, map[string]any{}, "", store.NewID()), 400, "INVALID_BODY")
	expectCode(t, requestAuth(f.http, "POST", path, "", testOrigin, "", f.reader.cookie), 403, "CSRF_REJECTED")
	if _, err := f.db.Exec(`CREATE TRIGGER fail_http_view BEFORE INSERT ON mutation_receipts WHEN NEW.operation='album.view.record' BEGIN SELECT RAISE(ABORT,'/private/SQL/view-receipt'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("POST", path, nil, "", store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	for _, secret := range []string{"/private", "SQL", "view-receipt", f.source} {
		if strings.Contains(problemContent(t, failed), secret) {
			t.Fatal("view failure exposed private implementation details", failed.Body.String())
		}
	}
	if current := decodeResponse[access.AlbumView](t, f.admin.request("GET", path, nil, "", ""), 200); !reflect.DeepEqual(current, admin) {
		t.Fatal("late receipt failure left a partial view", current, admin)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_http_view`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE users SET download_original=0 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, mediaRequest(f.reader, "GET", f.originalURL, nil), 404, "NOT_FOUND")
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	if value := decodeResponse[access.AlbumView](t, f.reader.request("POST", path, nil, "", store.NewID()), 200); value.ViewCount != 3 {
		t.Fatal("view metadata depended on a source mount", value)
	}
	if _, err := f.db.Exec(`DELETE FROM user_groups WHERE user_id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.reader.request("POST", path, nil, "", key), 404, "NOT_FOUND")
	for _, secret := range []string{f.source, f.cache, "root_path", "source_sha256", "csrf", "password", "user_id"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("view response disclosed unrelated private state", secret)
		}
	}
}
