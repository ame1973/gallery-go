package httpapi

import (
	"net/http"

	"gallerygo/internal/scanner"
)

type scanHandler struct{ service *scanner.Scans }

func (h scanHandler) enqueue(w http.ResponseWriter, r *http.Request) {
	var input scanner.ScanRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	job, err := h.service.Enqueue(r.Context(), currentSession(r), input, key)
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("Location", "/api/v2/admin/jobs/"+job.ID)
	w.Header().Set("ETag", resourceETag("job", job.ID, job.Version))
	writeJSON(w, http.StatusAccepted, job)
}
