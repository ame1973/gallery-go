package httpapi

import "net/http"

func (a authorization) albumFavorite(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		problem(w, 400, "INVALID_FILTER", "Personal favorite resources do not accept query parameters.")
		return
	}
	id := r.PathValue("album_id")
	value, err := a.service.AlbumFavorite(r.Context(), currentSession(r), id)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("favorite", personalResourceID(r, id), value.Version))
	writeJSON(w, http.StatusOK, value)
}

func (a authorization) setAlbumFavorite(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		problem(w, 400, "INVALID_FILTER", "Personal favorite resources do not accept query parameters.")
		return
	}
	id := r.PathValue("album_id")
	version, ok := resourcePrecondition(w, r, "favorite", personalResourceID(r, id))
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input struct {
		CollectionID *string `json:"collection_id" nullable:"true"`
	}
	favorited := r.Method == http.MethodPut
	if favorited {
		if !decodeJSON(w, r, &input) {
			return
		}
	} else if !requireEmptyBody(w, r) {
		return
	}
	value, err := a.service.SetAlbumFavorite(r.Context(), currentSession(r), id, version, favorited, input.CollectionID, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("favorite", personalResourceID(r, id), value.Version))
	writeJSON(w, http.StatusOK, value)
}
