package httpapi

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/store"
)

func TestFavoriteCollectionDeletionHTTPRequiresTwoStepOwnedConfirmation(t *testing.T) {
	f := protectedMedia(t)
	const base = "/api/v2/favorite-collections"
	rootResponse := f.reader.request("POST", base, map[string]string{"name": "Delete root"}, "", store.NewID())
	root := decodeResponse[access.FavoriteCollection](t, rootResponse, 201)
	rootPath := base + "/" + root.ID
	child := decodeResponse[access.FavoriteCollection](t, f.reader.request("POST", base, map[string]string{"name": "Child", "parent_id": root.ID}, "", store.NewID()), 201)
	favoritePath := "/api/v2/favorites/media-album"
	favorite := f.reader.request("GET", favoritePath, nil, "", "")
	assigned := decodeResponse[access.AlbumFavorite](t, f.reader.request("PUT", favoritePath, map[string]string{"collection_id": child.ID}, favorite.Header().Get("ETag"), store.NewID()), 200)
	if !assigned.Favorited || assigned.CollectionID == nil || *assigned.CollectionID != child.ID {
		t.Fatal("fixture did not assign an authorized favorite", assigned)
	}
	previewPath := rootPath + "/deletion-preview"
	guest := adminClient{handler: f.http}
	expectCode(t, guest.request("GET", previewPath+"?action=remove_favorites", nil, "", ""), 401, "UNAUTHENTICATED")
	expectCode(t, guest.request("DELETE", rootPath, map[string]string{}, rootResponse.Header().Get("ETag"), store.NewID()), 401, "UNAUTHENTICATED")
	expectCode(t, f.admin.request("GET", previewPath+"?action=remove_favorites", nil, "", ""), 404, "NOT_FOUND")
	for _, query := range []string{"", "?action=", "?action=remove_favorites&action=move_to_default", "?action=remove_favorites&user_id=other", "?Action=remove_favorites"} {
		expectCode(t, f.reader.request("GET", previewPath+query, nil, "", ""), 400, "INVALID_FILTER")
	}
	expectCode(t, f.reader.request("GET", previewPath+"?action=cancel", nil, "", ""), 422, "VALIDATION_FAILED")
	previewResponse := f.reader.request("GET", previewPath+"?action=remove_favorites", nil, "", "")
	preview := decodeResponse[access.FavoriteCollectionDeletionPreview](t, previewResponse, 200)
	if preview.Action != access.FavoriteDeleteRemove || len(preview.ConfirmationToken) != 64 || !preview.IncludesDescendants || !preview.HiddenItemsMayBeAffected || previewResponse.Header().Get("Cache-Control") != "private, no-store" || strings.Contains(previewResponse.Body.String(), "media-album") || strings.Contains(previewResponse.Body.String(), child.ID) {
		t.Fatal("preview leaked an identity or omitted an explicit warning", previewResponse.Body.String())
	}
	body := map[string]string{"action": access.FavoriteDeleteRemove, "confirmation_token": preview.ConfirmationToken}
	expectCode(t, f.reader.request("DELETE", rootPath, body, "", store.NewID()), 428, "PRECONDITION_REQUIRED")
	expectCode(t, f.reader.request("DELETE", rootPath, body, rootResponse.Header().Get("ETag"), ""), 422, "VALIDATION_FAILED")
	expectCode(t, f.reader.request("DELETE", rootPath+"?user_id=other", body, rootResponse.Header().Get("ETag"), store.NewID()), 400, "INVALID_FILTER")
	expectCode(t, f.reader.request("DELETE", rootPath, map[string]string{"action": access.FavoriteDeleteMoveToDefault, "confirmation_token": preview.ConfirmationToken}, rootResponse.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	for _, raw := range []string{
		`{"action":null,"confirmation_token":"` + preview.ConfirmationToken + `"}`,
		`{"action":"remove_favorites","confirmation_token":"` + preview.ConfirmationToken + `","user_id":"other"}`,
		`{"action":"remove_favorites","action":"move_to_default","confirmation_token":"` + preview.ConfirmationToken + `"}`,
	} {
		r := httptest.NewRequest("DELETE", rootPath, strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", testOrigin)
		r.Header.Set("X-CSRF-Token", f.reader.csrf)
		r.Header.Set("If-Match", rootResponse.Header().Get("ETag"))
		r.Header.Set("Idempotency-Key", store.NewID())
		r.AddCookie(f.reader.cookie)
		w := httptest.NewRecorder()
		f.http.ServeHTTP(w, r)
		expectCode(t, w, 400, "INVALID_JSON")
	}
	csrfMissing := httptest.NewRequest("DELETE", rootPath, strings.NewReader(`{}`))
	csrfMissing.Header.Set("Content-Type", "application/json")
	csrfMissing.Header.Set("Origin", testOrigin)
	csrfMissing.AddCookie(f.reader.cookie)
	csrfResponse := httptest.NewRecorder()
	f.http.ServeHTTP(csrfResponse, csrfMissing)
	expectCode(t, csrfResponse, 403, "CSRF_REJECTED")
	if w := f.reader.request("POST", base, map[string]string{"name": "Later child", "parent_id": root.ID}, "", store.NewID()); w.Code != 201 {
		t.Fatal("could not change subtree after confirmation", w.Code, w.Body.String())
	}
	expectCode(t, f.reader.request("DELETE", rootPath, body, rootResponse.Header().Get("ETag"), store.NewID()), 409, "VERSION_CONFLICT")
	fresh := decodeResponse[access.FavoriteCollectionDeletionPreview](t, f.reader.request("GET", previewPath+"?"+url.Values{"action": {access.FavoriteDeleteRemove}}.Encode(), nil, "", ""), 200)
	body["confirmation_token"] = fresh.ConfirmationToken
	key := store.NewID()
	deleted := f.reader.request("DELETE", rootPath, body, rootResponse.Header().Get("ETag"), key)
	if deleted.Code != 204 || deleted.Body.Len() != 0 || deleted.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("confirmed subtree deletion did not return private 204", deleted.Code, deleted.Header(), deleted.Body.String())
	}
	if replay := f.reader.request("DELETE", rootPath, body, rootResponse.Header().Get("ETag"), key); replay.Code != 204 || replay.Body.Len() != 0 {
		t.Fatal("historical delete receipt did not replay", replay.Code, replay.Body.String())
	}
	expectCode(t, f.reader.request("GET", rootPath, nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.reader.request("GET", base+"/"+child.ID, nil, "", ""), 404, "NOT_FOUND")
	cleared := decodeResponse[access.AlbumFavorite](t, f.reader.request("GET", favoritePath, nil, "", ""), 200)
	if cleared.Favorited || cleared.CollectionID != nil || cleared.Version != assigned.Version+1 {
		t.Fatal("remove choice lost the personal tombstone revision", cleared)
	}
}
