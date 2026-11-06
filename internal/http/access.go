package httpapi

import (
	"net/http"

	"gallerygo/internal/access"
)

type authorization struct{ service *access.Service }

type nameRequest struct {
	Name string `json:"name"`
}

type groupsRequest struct {
	GroupIDs []string `json:"group_ids"`
}

type albumPolicyRequest struct {
	Restricted *bool    `json:"restricted"`
	GroupIDs   []string `json:"group_ids"`
}

func (a authorization) groups(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	items, err := a.service.Groups(r.Context(), currentSession(r), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(group access.Group) string { return group.ID })
}

func (a authorization) group(w http.ResponseWriter, r *http.Request) {
	group, err := a.service.Group(r.Context(), currentSession(r), r.PathValue("group_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("group", group.ID, group.Version))
	writeJSON(w, 200, group)
}

func (a authorization) createGroup(w http.ResponseWriter, r *http.Request) {
	var input nameRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	group, err := a.service.CreateGroup(r.Context(), currentSession(r), input.Name, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("group", group.ID, group.Version))
	w.Header().Set("Location", "/api/v2/admin/groups/"+group.ID)
	writeJSON(w, 201, group)
}

func (a authorization) renameGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("group_id")
	version, ok := resourcePrecondition(w, r, "group", id)
	if !ok {
		return
	}
	var input nameRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	group, err := a.service.RenameGroup(r.Context(), currentSession(r), id, version, input.Name)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("group", id, group.Version))
	writeJSON(w, 200, group)
}

func (a authorization) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("group_id")
	version, ok := resourcePrecondition(w, r, "group", id)
	if !ok {
		return
	}
	if err := a.service.DeleteGroup(r.Context(), currentSession(r), id, version); err != nil {
		requestError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a authorization) userGroups(w http.ResponseWriter, r *http.Request) {
	membership, err := a.service.UserGroups(r.Context(), currentSession(r), r.PathValue("user_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("user", membership.UserID, membership.Version))
	writeJSON(w, 200, membership)
}

func (a authorization) setUserGroups(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("user_id")
	version, ok := userPrecondition(w, r)
	if !ok {
		return
	}
	var input groupsRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	membership, err := a.service.SetUserGroups(r.Context(), currentSession(r), id, version, input.GroupIDs)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("user", id, membership.Version))
	writeJSON(w, 200, membership)
}

func (a authorization) adminLibraries(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	items, err := a.service.AdminLibraries(r.Context(), currentSession(r), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(library access.Library) string { return library.ID })
}

func (a authorization) adminLibrary(w http.ResponseWriter, r *http.Request) {
	library, err := a.service.AdminLibrary(r.Context(), currentSession(r), r.PathValue("library_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("library", library.ID, library.Version))
	writeJSON(w, 200, library)
}

func (a authorization) createLibrary(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name   string `json:"name"`
		Active *bool  `json:"active"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	library, err := a.service.CreateLibrary(r.Context(), currentSession(r), input.Name, input.Active == nil || *input.Active, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("library", library.ID, library.Version))
	w.Header().Set("Location", "/api/v2/admin/libraries/"+library.ID)
	writeJSON(w, 201, library)
}

func (a authorization) updateLibrary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("library_id")
	version, ok := resourcePrecondition(w, r, "library", id)
	if !ok {
		return
	}
	var input access.LibraryPatch
	if !decodeJSON(w, r, &input) {
		return
	}
	library, err := a.service.UpdateLibrary(r.Context(), currentSession(r), id, version, input)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("library", id, library.Version))
	writeJSON(w, 200, library)
}

func (a authorization) deleteLibrary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("library_id")
	version, ok := resourcePrecondition(w, r, "library", id)
	if !ok {
		return
	}
	if err := a.service.DeleteLibrary(r.Context(), currentSession(r), id, version); err != nil {
		requestError(w, err)
		return
	}
	w.WriteHeader(204)
}

func (a authorization) libraryPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := a.service.LibraryPolicy(r.Context(), currentSession(r), r.PathValue("library_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("library", policy.LibraryID, policy.Version))
	writeJSON(w, 200, policy)
}

func (a authorization) setLibraryPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("library_id")
	version, ok := resourcePrecondition(w, r, "library", id)
	if !ok {
		return
	}
	var input groupsRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	policy, err := a.service.SetLibraryPolicy(r.Context(), currentSession(r), id, version, input.GroupIDs)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("library", id, policy.Version))
	writeJSON(w, 200, policy)
}

func (a authorization) albumPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := a.service.AlbumPolicy(r.Context(), currentSession(r), r.PathValue("album_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("album", policy.AlbumID, policy.Version))
	writeJSON(w, 200, policy)
}

func (a authorization) setAlbumPolicy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("album_id")
	version, ok := resourcePrecondition(w, r, "album", id)
	if !ok {
		return
	}
	var input albumPolicyRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Restricted == nil {
		problem(w, 422, "VALIDATION_FAILED", "The restricted field and an explicit group_ids array are required.")
		return
	}
	policy, err := a.service.SetAlbumPolicy(r.Context(), currentSession(r), id, version, *input.Restricted, input.GroupIDs)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("album", id, policy.Version))
	writeJSON(w, 200, policy)
}

func (a authorization) libraries(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	items, err := a.service.Libraries(r.Context(), currentSession(r), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(library access.Library) string { return library.ID })
}

func (a authorization) library(w http.ResponseWriter, r *http.Request) {
	library, err := a.service.Library(r.Context(), currentSession(r), r.PathValue("library_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("library", library.ID, library.Version))
	writeJSON(w, 200, library)
}

func (a authorization) album(w http.ResponseWriter, r *http.Request) {
	album, err := a.service.Album(r.Context(), currentSession(r), r.PathValue("album_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("album", album.ID, album.Version))
	writeJSON(w, 200, album)
}
