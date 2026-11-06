package httpapi

import (
	"net/http"

	"gallerygo/internal/access"
)

type albumTagsRequest struct {
	TagIDs []string `json:"tag_ids"`
}

func (a authorization) albumTags(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	for key := range page.query {
		if key != "limit" && key != "cursor" {
			problem(w, 400, "INVALID_FILTER", "Album tag pages support only limit and cursor.")
			return
		}
	}
	items, err := a.service.AlbumTags(r.Context(), currentSession(r), r.PathValue("album_id"), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	// Tag labels have their own versions; an album-only ETag would be misleading.
	writePage(w, page, items, func(tag access.Tag) string { return tag.ID })
}

func (a authorization) adminAlbumTags(w http.ResponseWriter, r *http.Request) {
	value, err := a.service.AdminAlbumTags(r.Context(), currentSession(r), r.PathValue("album_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("album", value.AlbumID, value.Version))
	writeJSON(w, 200, value)
}

func (a authorization) setAlbumTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("album_id")
	version, ok := resourcePrecondition(w, r, "album", id)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input albumTagsRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	value, err := a.service.SetAlbumTags(r.Context(), currentSession(r), id, version, input.TagIDs, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("album", value.AlbumID, value.Version))
	writeJSON(w, 200, value)
}
