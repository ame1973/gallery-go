package httpapi

import (
	"net/http"

	"gallerygo/internal/access"
)

func (a authorization) randomAlbums(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	if len(page.query) > 1 || len(page.query) == 1 && !page.query.Has("limit") {
		problem(w, 400, "INVALID_FILTER", "Only the optional sample limit is supported.")
		return
	}
	items, err := a.service.RandomAlbums(r.Context(), currentSession(r), page.limit)
	if err != nil {
		requestError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Items []access.Album `json:"items"`
	}{Items: items})
}
