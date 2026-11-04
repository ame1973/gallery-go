package httpapi

import (
	"io"
	"net/http"
	"strconv"

	"gallerygo/internal/jobs"
)

type jobHandler struct{ service *jobs.Admin }

func (h jobHandler) list(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	if len(page.query["status"]) > 1 {
		problem(w, 400, "INVALID_FILTER", "Use at most one job status filter.")
		return
	}
	items, err := h.service.List(r.Context(), currentSession(r), page.cursor.After, page.query.Get("status"), page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(job jobs.Job) string { return job.ID })
}

func (h jobHandler) get(w http.ResponseWriter, r *http.Request) {
	job, err := h.service.Get(r.Context(), currentSession(r), r.PathValue("job_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("job", job.ID, job.Version))
	writeJSON(w, 200, job)
}

func (h jobHandler) attempts(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	after := 0
	if page.cursor.After != "" {
		var err error
		after, err = strconv.Atoi(page.cursor.After)
		if err != nil || after < 1 || after > 100 {
			problem(w, 400, "INVALID_CURSOR", "The attempt cursor is invalid.")
			return
		}
	}
	items, err := h.service.Attempts(r.Context(), currentSession(r), r.PathValue("job_id"), after, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(attempt jobs.Attempt) string { return strconv.Itoa(attempt.Number) })
}

func (h jobHandler) cancel(w http.ResponseWriter, r *http.Request) { h.mutate(w, r, false) }
func (h jobHandler) retry(w http.ResponseWriter, r *http.Request)  { h.mutate(w, r, true) }

func (h jobHandler) mutate(w http.ResponseWriter, r *http.Request, retry bool) {
	version, ok := resourcePrecondition(w, r, "job", r.PathValue("job_id"))
	if !ok {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	// These commands have no options. Reject even a small JSON body rather than
	// silently accepting misspelled fields or private worker capabilities.
	if r.Body != nil {
		data, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(data) != 0 {
			problem(w, 400, "INVALID_BODY", "This job command does not accept a request body.")
			return
		}
	}
	var job jobs.Job
	var err error
	status := http.StatusOK
	if retry {
		job, err = h.service.Retry(r.Context(), currentSession(r), r.PathValue("job_id"), version, key)
		status = http.StatusAccepted
	} else {
		job, err = h.service.Cancel(r.Context(), currentSession(r), r.PathValue("job_id"), version, key)
	}
	if err != nil {
		requestError(w, err)
		return
	}
	w.Header().Set("ETag", resourceETag("job", job.ID, job.Version))
	writeJSON(w, status, job)
}
