package httpapi

import (
	"net/http"

	"gallerygo/internal/access"
)

type tagBatchHandler struct{ service *access.TagBatches }

func (h tagBatchHandler) enqueue(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var input access.TagBatchRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	job, err := h.service.Enqueue(r.Context(), currentSession(r), input, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("job", job.ID, job.Version))
	w.Header().Set("Location", "/api/v2/admin/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, job)
}

func (h tagBatchHandler) targets(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	for key := range page.query {
		if key != "limit" && key != "cursor" {
			problem(w, 400, "INVALID_FILTER", "Batch target pages support only limit and cursor.")
			return
		}
	}
	items, err := h.service.Targets(r.Context(), currentSession(r), r.PathValue("job_id"), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(target access.TagBatchTarget) string { return target.AlbumID })
}

func (h tagBatchHandler) results(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	for key := range page.query {
		if key != "limit" && key != "cursor" {
			problem(w, 400, "INVALID_FILTER", "Batch result pages support only limit and cursor.")
			return
		}
	}
	items, err := h.service.Results(r.Context(), currentSession(r), r.PathValue("job_id"), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(result access.TagBatchResult) string { return result.ID })
}
