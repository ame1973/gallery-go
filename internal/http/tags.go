package httpapi

import (
	"net/http"

	"gallerygo/internal/access"
)

func (a authorization) tags(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	for key := range page.query {
		if key != "limit" && key != "cursor" {
			problem(w, 400, "INVALID_FILTER", "Tag catalog pages support only limit and cursor.")
			return
		}
	}
	items, err := a.service.Tags(r.Context(), currentSession(r), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(tag access.Tag) string { return tag.ID })
}

func (a authorization) tag(w http.ResponseWriter, r *http.Request) {
	value, err := a.service.Tag(r.Context(), currentSession(r), r.PathValue("tag_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("tag", value.ID, value.Version))
	writeJSON(w, 200, value)
}

func (a authorization) createTag(w http.ResponseWriter, r *http.Request) {
	var input nameRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	value, err := a.service.CreateTag(r.Context(), currentSession(r), input.Name, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("tag", value.ID, value.Version))
	w.Header().Set("Location", "/api/v2/admin/tags/"+value.ID)
	writeJSON(w, 201, value)
}

func (a authorization) renameTag(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("tag_id")
	version, ok := resourcePrecondition(w, r, "tag", id)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input nameRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	value, err := a.service.RenameTag(r.Context(), currentSession(r), id, version, input.Name, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("tag", value.ID, value.Version))
	writeJSON(w, 200, value)
}

func (a authorization) deleteTag(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("tag_id")
	version, ok := resourcePrecondition(w, r, "tag", id)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok || !requireEmptyBody(w, r) {
		return
	}
	if err := a.service.DeleteTag(r.Context(), currentSession(r), id, version, key); err != nil {
		requestError(w, err)
		return
	}
	w.WriteHeader(204)
}
