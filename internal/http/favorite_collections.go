package httpapi

import (
	"net/http"
	"net/url"

	"gallerygo/internal/access"
)

func (a authorization) favoriteCollections(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	for key, values := range page.query {
		if key == "limit" || key == "cursor" {
			continue
		}
		if (key != "parent_id" && key != "root") || len(values) != 1 || values[0] == "" {
			problem(w, 400, "INVALID_FILTER", "Use one parent_id or root=true with limit and cursor.")
			return
		}
	}
	if page.query.Has("cursor") && page.query.Get("cursor") == "" || page.cursor.Position != "" {
		problem(w, 400, "INVALID_CURSOR", "Use a valid collection page continuation.")
		return
	}
	filter := access.FavoriteCollectionFilter{}
	if page.query.Has("parent_id") {
		filter.ParentID = new(page.query.Get("parent_id"))
	}
	if page.query.Has("root") {
		if page.query.Get("root") != "true" {
			problem(w, 400, "INVALID_FILTER", "Use root=true or omit root.")
			return
		}
		filter.RootOnly = true
	}
	if err := filter.Validate(); err != nil {
		problem(w, 400, "INVALID_FILTER", "Use one bounded parent_id or root=true, not both.")
		return
	}
	items, err := a.service.FavoriteCollections(r.Context(), currentSession(r), filter, page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(value access.FavoriteCollection) string { return value.ID })
}

func (a authorization) favoriteCollection(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		problem(w, 400, "INVALID_FILTER", "Personal collection resources do not accept query parameters.")
		return
	}
	id := r.PathValue("collection_id")
	value, err := a.service.FavoriteCollection(r.Context(), currentSession(r), id)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("favorite_collection", personalResourceID(r, id), value.Version))
	writeJSON(w, 200, value)
}

func (a authorization) createFavoriteCollection(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		problem(w, 400, "INVALID_FILTER", "Creating a personal collection does not accept query parameters.")
		return
	}
	var input struct {
		Name     string  `json:"name"`
		ParentID *string `json:"parent_id" nullable:"true"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	value, err := a.service.CreateFavoriteCollection(r.Context(), currentSession(r), input.Name, input.ParentID, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("favorite_collection", personalResourceID(r, value.ID), value.Version))
	w.Header().Set("Location", "/api/v2/favorite-collections/"+url.PathEscape(value.ID))
	writeJSON(w, 201, value)
}

func (a authorization) renameFavoriteCollection(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		problem(w, 400, "INVALID_FILTER", "Personal collection resources do not accept query parameters.")
		return
	}
	id := r.PathValue("collection_id")
	version, ok := resourcePrecondition(w, r, "favorite_collection", personalResourceID(r, id))
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
	value, err := a.service.RenameFavoriteCollection(r.Context(), currentSession(r), id, version, input.Name, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("favorite_collection", personalResourceID(r, id), value.Version))
	writeJSON(w, 200, value)
}
