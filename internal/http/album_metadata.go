package httpapi

import (
	"net/http"

	"gallerygo/internal/access"
)

func (a authorization) albumMetadata(w http.ResponseWriter, r *http.Request) {
	value, err := a.service.AdminAlbumMetadata(r.Context(), currentSession(r), r.PathValue("album_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("album", value.ID, value.Version))
	writeJSON(w, 200, value)
}

func (a authorization) updateAlbumMetadata(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("album_id")
	version, ok := resourcePrecondition(w, r, "album", id)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input access.AlbumMetadataPatch
	if !decodeJSON(w, r, &input) {
		return
	}
	value, err := a.service.UpdateAlbumMetadata(r.Context(), currentSession(r), id, version, input, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("album", value.ID, value.Version))
	writeJSON(w, 200, value)
}
