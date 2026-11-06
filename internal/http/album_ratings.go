package httpapi

import (
	"encoding/base64"
	"net/http"
)

// Bind the validator to both account and album, not just equal vote revisions.
func ratingResourceID(r *http.Request, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(currentSession(r).User.ID + "\x00" + id))
}

func (a authorization) albumRating(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		problem(w, 400, "INVALID_FILTER", "Personal rating resources do not accept query parameters.")
		return
	}
	id := r.PathValue("album_id")
	value, err := a.service.AlbumRating(r.Context(), currentSession(r), id)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("rating", ratingResourceID(r, id), value.Version))
	writeJSON(w, http.StatusOK, value)
}

func (a authorization) setAlbumRating(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		problem(w, 400, "INVALID_FILTER", "Personal rating resources do not accept query parameters.")
		return
	}
	id := r.PathValue("album_id")
	version, ok := resourcePrecondition(w, r, "rating", ratingResourceID(r, id))
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var rating *int
	if r.Method == http.MethodPut {
		var input struct {
			Rating int `json:"rating"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		rating = &input.Rating
	} else if !requireEmptyBody(w, r) {
		return
	}
	value, err := a.service.SetAlbumRating(r.Context(), currentSession(r), id, version, rating, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("rating", ratingResourceID(r, id), value.Version))
	writeJSON(w, http.StatusOK, value)
}
