package httpapi

import (
	"net/http"

	"gallerygo/internal/access"
)

func personPage(w http.ResponseWriter, r *http.Request) (pagination, bool) {
	page, ok := readPage(w, r)
	if !ok {
		return page, false
	}
	for key := range page.query {
		if key != "limit" && key != "cursor" {
			problem(w, 400, "INVALID_FILTER", "Person pages currently support only limit and cursor.")
			return page, false
		}
	}
	return page, true
}

func (a authorization) people(w http.ResponseWriter, r *http.Request) {
	page, ok := personPage(w, r)
	if !ok {
		return
	}
	items, err := a.service.People(r.Context(), currentSession(r), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(person access.Person) string { return person.ID })
}

func (a authorization) person(w http.ResponseWriter, r *http.Request) {
	person, err := a.service.Person(r.Context(), currentSession(r), r.PathValue("person_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	writeJSON(w, 200, person)
}

func (a authorization) personMetadata(w http.ResponseWriter, r *http.Request) {
	value, err := a.service.AdminPersonMetadata(r.Context(), currentSession(r), r.PathValue("person_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("person", value.ID, value.Version))
	writeJSON(w, 200, value)
}

func (a authorization) updatePersonMetadata(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("person_id")
	version, ok := resourcePrecondition(w, r, "person", id)
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input access.PersonMetadataPatch
	if !decodeJSON(w, r, &input) {
		return
	}
	value, err := a.service.UpdatePersonMetadata(r.Context(), currentSession(r), id, version, input, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("person", value.ID, value.Version))
	writeJSON(w, 200, value)
}

func (a authorization) personAlbums(w http.ResponseWriter, r *http.Request) {
	page, ok := personPage(w, r)
	if !ok {
		return
	}
	items, err := a.service.PersonAlbums(r.Context(), currentSession(r), r.PathValue("person_id"), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(album access.Album) string { return album.ID })
}
