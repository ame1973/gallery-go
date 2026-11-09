package httpapi

import "net/http"

func (a authorization) albumView(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		problem(w, 400, "INVALID_FILTER", "Personal view resources do not accept query parameters.")
		return
	}
	id := r.PathValue("album_id")
	value, err := a.service.AlbumView(r.Context(), currentSession(r), id)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("album_view", personalResourceID(r, id), value.Version))
	writeJSON(w, http.StatusOK, value)
}

func (a authorization) recordAlbumView(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		problem(w, 400, "INVALID_FILTER", "Recording a view does not accept query parameters.")
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok || !requireEmptyBody(w, r) {
		return
	}
	id := r.PathValue("album_id")
	value, err := a.service.RecordAlbumView(r.Context(), currentSession(r), id, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("album_view", personalResourceID(r, id), value.Version))
	writeJSON(w, http.StatusOK, value)
}
