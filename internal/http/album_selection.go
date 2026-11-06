package httpapi

import (
	"net/http"

	"gallerygo/internal/access"
)

func albumSelectionPage(w http.ResponseWriter, r *http.Request) (pagination, access.AlbumFilter, bool) {
	page, ok := readPage(w, r)
	if !ok {
		return page, access.AlbumFilter{}, false
	}
	if (page.query.Has("cursor") && page.query.Get("cursor") == "") || page.cursor.Position != "" {
		problem(w, 400, "INVALID_CURSOR", "Use a nonempty album-ID continuation without an unrelated sort position.")
		return page, access.AlbumFilter{}, false
	}
	for key, values := range page.query {
		if key == "limit" || key == "cursor" {
			continue
		}
		if (key != "library_id" && key != "source_id" && key != "person_id" && key != "tag_id") || len(values) != 1 || values[0] == "" {
			problem(w, 400, "INVALID_FILTER", "Use one nonempty library_id, source_id, person_id or tag_id selector, with limit and cursor.")
			return page, access.AlbumFilter{}, false
		}
	}
	filter := access.AlbumFilter{
		LibraryID: page.query.Get("library_id"), SourceID: page.query.Get("source_id"),
		PersonID: page.query.Get("person_id"), TagID: page.query.Get("tag_id"),
	}
	if err := filter.Validate(); err != nil {
		requestError(w, err)
		return page, filter, false
	}
	return page, filter, true
}

func (a authorization) albums(w http.ResponseWriter, r *http.Request) {
	page, filter, ok := albumSelectionPage(w, r)
	if !ok {
		return
	}
	items, err := a.service.Albums(r.Context(), currentSession(r), filter, page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(album access.Album) string { return album.ID })
}
