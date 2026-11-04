package httpapi

import (
	"net/http"

	"gallerygo/internal/library"
)

type sourceHandler struct{ service *library.Sources }

func (h sourceHandler) roots(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	items, err := h.service.Roots(r.Context(), currentSession(r), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(root library.Root) string { return root.Name })
}

func (h sourceHandler) list(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	items, err := h.service.List(r.Context(), currentSession(r), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(source library.Source) string { return source.ID })
}

func writeSource(w http.ResponseWriter, status int, source library.Source) {
	w.Header().Set("ETag", resourceETag("source", source.ID, source.Version))
	writeJSON(w, status, source)
}

func (h sourceHandler) get(w http.ResponseWriter, r *http.Request) {
	source, err := h.service.Get(r.Context(), currentSession(r), r.PathValue("source_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	writeSource(w, 200, source)
}

type createSourceRequest struct {
	LibraryID               string           `json:"library_id"`
	RootName                string           `json:"root_name"`
	Mapping                 *library.Mapping `json:"mapping"`
	Active                  *bool            `json:"active"`
	MinimumFiles            *int64           `json:"minimum_files"`
	PollIntervalSeconds     *int64           `json:"poll_interval_seconds"`
	FullScanIntervalSeconds *int64           `json:"full_scan_interval_seconds"`
}

func (h sourceHandler) create(w http.ResponseWriter, r *http.Request) {
	var input createSourceRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	settings := library.DefaultSourceSettings()
	settings.LibraryID, settings.RootName = input.LibraryID, input.RootName
	if input.Mapping != nil {
		settings.Mapping = *input.Mapping
	}
	if input.Active != nil {
		settings.Active = *input.Active
	}
	if input.MinimumFiles != nil {
		settings.MinimumFiles = *input.MinimumFiles
	}
	if input.PollIntervalSeconds != nil {
		settings.PollIntervalSeconds = *input.PollIntervalSeconds
	}
	if input.FullScanIntervalSeconds != nil {
		settings.FullScanIntervalSeconds = *input.FullScanIntervalSeconds
	}
	source, err := h.service.Create(r.Context(), currentSession(r), settings, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v2/admin/sources/"+source.ID)
	writeSource(w, 201, source)
}

func (h sourceHandler) update(w http.ResponseWriter, r *http.Request) {
	version, ok := resourcePrecondition(w, r, "source", r.PathValue("source_id"))
	if !ok {
		return
	}
	var patch library.SourcePatch
	if !decodeJSON(w, r, &patch) {
		return
	}
	source, err := h.service.Update(r.Context(), currentSession(r), r.PathValue("source_id"), version, patch)
	if err != nil {
		requestError(w, err)
		return
	}
	writeSource(w, 200, source)
}

func (h sourceHandler) remove(w http.ResponseWriter, r *http.Request) {
	version, ok := resourcePrecondition(w, r, "source", r.PathValue("source_id"))
	if !ok {
		return
	}
	if err := h.service.DeleteEmpty(r.Context(), currentSession(r), r.PathValue("source_id"), version); err != nil {
		requestError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h sourceHandler) check(w http.ResponseWriter, r *http.Request) {
	version, ok := resourcePrecondition(w, r, "source", r.PathValue("source_id"))
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	if !requireEmptyBody(w, r) {
		return
	}
	source, err := h.service.Check(r.Context(), currentSession(r), r.PathValue("source_id"), version, key)
	if err != nil {
		requestError(w, err)
		return
	}
	writeSource(w, 200, source)
}
