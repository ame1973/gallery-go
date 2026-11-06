package httpapi

import (
	"net/http"
	"strconv"

	"gallerygo/internal/access"
)

func albumSelectionPage(w http.ResponseWriter, r *http.Request) (pagination, access.AlbumFilter, bool) {
	page, ok := readPage(w, r)
	if !ok {
		return page, access.AlbumFilter{}, false
	}
	if page.query.Has("cursor") && page.query.Get("cursor") == "" {
		problem(w, 400, "INVALID_CURSOR", "Use a nonempty album page continuation.")
		return page, access.AlbumFilter{}, false
	}
	for key, values := range page.query {
		if key == "limit" || key == "cursor" {
			continue
		}
		if (key != "library_id" && key != "source_id" && key != "person_id" && key != "tag_id" && key != "sort") || len(values) != 1 || values[0] == "" {
			problem(w, 400, "INVALID_FILTER", "Use one nonempty value for each resource ID or latest/oldest sort, with limit and cursor.")
			return page, access.AlbumFilter{}, false
		}
	}
	filter := access.AlbumFilter{
		LibraryID: page.query.Get("library_id"), SourceID: page.query.Get("source_id"),
		PersonID: page.query.Get("person_id"), TagID: page.query.Get("tag_id"), Sort: page.query.Get("sort"),
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
	after := access.AlbumPosition{ID: page.cursor.After, Indexed: page.cursor.Position}
	items, err := a.service.Albums(r.Context(), currentSession(r), filter, after, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	if len(items) > page.limit && filter.Sort != "" {
		page.cursor.Position = strconv.FormatInt(items[page.limit-1].IndexedAt.UnixMilli(), 10)
	}
	writePage(w, page, items, func(album access.Album) string { return album.ID })
}
