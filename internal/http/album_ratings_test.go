package httpapi

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func TestAlbumRatingsHTTPPrivateValidatorsReplacementClearAndOutageRetention(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/albums/media-album/rating"
	initial := f.reader.request("GET", path, nil, "", "")
	before := decodeResponse[access.AlbumRating](t, initial, 200)
	adminInitial := f.admin.request("GET", path, nil, "", "")
	adminBefore := decodeResponse[access.AlbumRating](t, adminInitial, 200)
	if before.Rating != nil || before.Version != 1 || before.UpdatedAt != nil || initial.Header().Get("ETag") == "" || initial.Header().Get("ETag") == adminInitial.Header().Get("ETag") || adminBefore.Version != 1 || initial.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("private initial rating was invented/unversioned or reused another account's validator", before, initial.Header(), adminInitial.Header())
	}
	key := store.NewID()
	body := map[string]int{"rating": 5}
	response := f.reader.request("PUT", path, body, initial.Header().Get("ETag"), key)
	changed := decodeResponse[access.AlbumRating](t, response, 200)
	if changed.Rating == nil || *changed.Rating != 5 || changed.Version != 2 || changed.UpdatedAt == nil || response.Header().Get("ETag") == initial.Header().Get("ETag") {
		t.Fatal("PUT did not publish this account's vote/version/time", changed, response.Header())
	}
	public := decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	if public.RatingAvg == nil || *public.RatingAvg != 5 || public.Version != 2 {
		t.Fatal("rating did not refresh the shared album average/version", public)
	}
	// Another voter changes the aggregate but not this account's private revision.
	adminChanged := f.admin.request("PUT", path, map[string]int{"rating": 1}, adminInitial.Header().Get("ETag"), store.NewID())
	decodeResponse[access.AlbumRating](t, adminChanged, 200)
	current := f.reader.request("GET", path, nil, "", "")
	if value := decodeResponse[access.AlbumRating](t, current, 200); !reflect.DeepEqual(value, changed) || current.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("another voter's write invalidated or leaked through the private state", value, changed)
	}
	public = decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	if public.RatingAvg == nil || *public.RatingAvg != 3 || public.Version != 3 {
		t.Fatal("two-account aggregate was not exact", public)
	}
	noop := f.reader.request("PUT", path, body, response.Header().Get("ETag"), store.NewID())
	if value := decodeResponse[access.AlbumRating](t, noop, 200); !reflect.DeepEqual(value, changed) || noop.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("same vote changed its state or validator", value, changed)
	}
	clearedResponse := f.reader.request("DELETE", path, nil, response.Header().Get("ETag"), store.NewID())
	cleared := decodeResponse[access.AlbumRating](t, clearedResponse, 200)
	if cleared.Rating != nil || cleared.Version != 3 || cleared.UpdatedAt == nil {
		t.Fatal("DELETE did not return a versioned null tombstone", cleared)
	}
	replay := f.reader.request("PUT", path, body, initial.Header().Get("ETag"), key)
	if value := decodeResponse[access.AlbumRating](t, replay, 200); !reflect.DeepEqual(value, changed) || replay.Header().Get("ETag") != response.Header().Get("ETag") {
		t.Fatal("historical retry was recomputed after clear", value, changed)
	}
	if value := decodeResponse[access.AlbumRating](t, f.reader.request("GET", path, nil, "", ""), 200); !reflect.DeepEqual(value, cleared) {
		t.Fatal("historical retry restored the cleared vote", value)
	}
	expectCode(t, f.reader.request("PUT", path, body, initial.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	expectCode(t, f.reader.request("PUT", path, map[string]int{"rating": 4}, initial.Header().Get("ETag"), key), 409, "IDEMPOTENCY_CONFLICT")
	public = decodeResponse[access.Album](t, f.reader.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	if public.RatingAvg == nil || *public.RatingAvg != 1 || public.Version != 4 {
		t.Fatal("cleared vote remained in the shared aggregate", public)
	}
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	unmounted := decodeResponse[access.AlbumRating](t, f.reader.request("PUT", path, map[string]int{"rating": 3}, clearedResponse.Header().Get("ETag"), store.NewID()), 200)
	if unmounted.Rating == nil || *unmounted.Rating != 3 || unmounted.Version != 4 {
		t.Fatal("private metadata vote depended on source reachability", unmounted)
	}
	for _, secret := range []string{f.source, f.cache, "root_path", "source_sha256", "csrf", "password", "user_id"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatal("personal rating response leaked unrelated authority/private source data", secret)
		}
	}
}

func TestAlbumRatingsHTTPStrictInputsOwnPreconditionsAndMutationProtection(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/albums/media-album/rating"
	initial := f.reader.request("GET", path, nil, "", "")
	decodeResponse[access.AlbumRating](t, initial, 200)
	tag := initial.Header().Get("ETag")
	otherTag := f.admin.request("GET", path, nil, "", "").Header().Get("ETag")
	body := map[string]int{"rating": 5}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		expectCode(t, f.reader.request(method, path+"?user_id=other", body, tag, store.NewID()), 400, "INVALID_FILTER")
	}
	guest := adminClient{handler: f.http}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		var input any
		if method == "PUT" {
			input = body
		}
		expectCode(t, guest.request(method, path, input, tag, store.NewID()), 401, "UNAUTHENTICATED")
	}
	expectCode(t, f.reader.request("PUT", path, body, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
	expectCode(t, f.reader.request("PUT", path, body, tag, ""), 422, "VALIDATION_FAILED")
	for _, invalid := range []string{"*", "W/" + tag, tag + "," + tag, otherTag, `"album:media-album:1"`, strings.Replace(tag, ":1\"", ":0\"", 1)} {
		expectCode(t, f.reader.request("PUT", path, body, invalid, store.NewID()), 400, "INVALID_PRECONDITION")
	}
	for _, input := range []map[string]any{{}, {"rating": 0}, {"rating": 6}, {"rating": -1}} {
		expectCode(t, f.reader.request("PUT", path, input, tag, store.NewID()), 422, "VALIDATION_FAILED")
	}
	for _, raw := range []string{`{"rating":null}`, `{"rating":true}`, `{"rating":"5"}`, `{"rating":1.0}`, `{"rating":1e0}`, `{"rating":1.5}`, `{"rating":2,"rating":5}`, `{"Rating":5}`, `{"rating":5,"user_id":"other"}`, `{"rating":5,"updated_at":"2000-01-01T00:00:00Z"}`, `[]`, `{"rating":5} {}`} {
		r := httptest.NewRequest("PUT", path, strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("X-CSRF-Token", f.reader.csrf)
		r.Header.Set("If-Match", tag)
		r.Header.Set("Idempotency-Key", store.NewID())
		r.AddCookie(f.reader.cookie)
		w := httptest.NewRecorder()
		f.http.ServeHTTP(w, r)
		expectCode(t, w, 400, "INVALID_JSON")
	}
	expectCode(t, f.reader.request("DELETE", path, map[string]any{}, tag, store.NewID()), 400, "INVALID_BODY")
	for _, method := range []string{"PUT", "DELETE"} {
		expectCode(t, requestAuth(f.http, method, path, `{"rating":5}`, testOrigin, "", f.reader.cookie), 403, "CSRF_REJECTED")
	}
	if state := decodeResponse[access.AlbumRating](t, f.reader.request("GET", path, nil, "", ""), 200); state.Rating != nil || state.Version != 1 {
		t.Fatal("invalid requests changed a personal vote", state)
	}
}

func TestAlbumRatingsHTTPFreshGrantsAccountDeleteAndPrivateRollback(t *testing.T) {
	f := protectedMedia(t)
	const path = "/api/v2/albums/media-album/rating"
	initial := f.reader.request("GET", path, nil, "", "")
	tag := initial.Header().Get("ETag")
	body, key := map[string]int{"rating": 5}, store.NewID()
	changed := f.reader.request("PUT", path, body, tag, key)
	decodeResponse[access.AlbumRating](t, changed, 200)
	if _, err := f.db.Exec(`DELETE FROM user_groups WHERE user_id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.reader.request("PUT", path, body, tag, key), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES(?,'media-group')`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	// The account DELETE endpoint is another real producer of the shared average.
	userPath := "/api/v2/admin/users/" + f.user.ID
	userResponse := f.admin.request("GET", userPath, nil, "", "")
	decodeResponse[auth.User](t, userResponse, 200)
	if w := f.admin.request("DELETE", userPath, nil, userResponse.Header().Get("ETag"), ""); w.Code != 204 {
		t.Fatal("account deletion failed", w.Code, w.Body.String())
	}
	public := decodeResponse[access.Album](t, f.admin.request("GET", "/api/v2/albums/media-album", nil, "", ""), 200)
	if public.RatingAvg != nil || public.Version != 3 {
		t.Fatal("real user deletion did not clear and version its shared vote", public)
	}
	expectCode(t, f.reader.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
	adminInitial := f.admin.request("GET", path, nil, "", "")
	if _, err := f.db.Exec(`CREATE TRIGGER fail_http_rating BEFORE INSERT ON mutation_receipts WHEN NEW.operation='album.rating.replace' BEGIN SELECT RAISE(ABORT,'/private/SQL/rating-receipt'); END`); err != nil {
		t.Fatal(err)
	}
	failed := f.admin.request("PUT", path, body, adminInitial.Header().Get("ETag"), store.NewID())
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	for _, secret := range []string{"/private", "SQL", "rating-receipt", f.source} {
		if strings.Contains(failed.Body.String(), secret) {
			t.Fatal("late rating failure leaked private implementation detail", failed.Body.String())
		}
	}
	value := decodeResponse[access.AlbumRating](t, f.admin.request("GET", path, nil, "", ""), 200)
	if value.Rating != nil || value.Version != 1 {
		t.Fatal("failed receipt retained a personal vote", value)
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(failed.Body.Bytes(), &payload) != nil || payload["request_id"] == nil {
		t.Fatal("rating failure omitted the existing Problem JSON request ID")
	}
	// Reject a malformed legacy instant before any successful JSON headers/body.
	if _, err := f.db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at,version)
 SELECT id,'media-album',5,?,2 FROM users WHERE role='admin'`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	invalid := f.admin.request("GET", path, nil, "", "")
	expectCode(t, invalid, 500, "INTERNAL_ERROR")
	if invalid.Header().Get("ETag") != "" || strings.Contains(invalid.Body.String(), "stored rating") || strings.Contains(invalid.Body.String(), "UTC range") {
		t.Fatal("invalid stored update time emitted a partial success or private detail", invalid.Header(), invalid.Body.String())
	}
}
