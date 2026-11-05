package httpapi

import (
	"net/http"

	"gallerygo/internal/media"
)

func duplicatePage(w http.ResponseWriter, r *http.Request) (pagination, media.DuplicateFilter, bool) {
	page, ok := readPage(w, r)
	if !ok {
		return page, media.DuplicateFilter{}, false
	}
	for key, values := range page.query {
		if key == "limit" || key == "cursor" {
			continue
		}
		if (key != "library_id" && key != "source_id") || len(values) != 1 || values[0] == "" {
			problem(w, 400, "INVALID_FILTER", "Use at most one nonempty value per library_id/source_id filter, with limit and cursor.")
			return page, media.DuplicateFilter{}, false
		}
	}
	filter := media.DuplicateFilter{LibraryID: page.query.Get("library_id"), SourceID: page.query.Get("source_id")}
	if err := filter.Validate(); err != nil {
		problem(w, 400, "INVALID_FILTER", "Duplicate filter IDs must be bounded, valid identifiers.")
		return page, media.DuplicateFilter{}, false
	}
	return page, filter, true
}

func (h mediaHandler) exactDuplicates(w http.ResponseWriter, r *http.Request) {
	page, filter, ok := duplicatePage(w, r)
	if !ok {
		return
	}
	items, err := h.service.ExactDuplicates(r.Context(), currentSession(r), filter, page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(group media.ExactDuplicateGroup) string { return group.ID })
}

func (h mediaHandler) exactDuplicateMedia(w http.ResponseWriter, r *http.Request) {
	page, filter, ok := duplicatePage(w, r)
	if !ok {
		return
	}
	items, err := h.service.ExactDuplicateMedia(r.Context(), currentSession(r), r.PathValue("group_id"), filter, page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(item media.Metadata) string { return item.ID })
}
