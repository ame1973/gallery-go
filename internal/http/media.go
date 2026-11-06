package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gallerygo/internal/media"
)

type mediaHandler struct{ service *media.Service }

func (h mediaHandler) get(w http.ResponseWriter, r *http.Request) {
	item, err := h.service.Get(r.Context(), currentSession(r), r.PathValue("media_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	writeJSON(w, 200, item)
}

func (h mediaHandler) variants(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	items, err := h.service.Variants(r.Context(), currentSession(r), r.PathValue("media_id"), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(item media.Variant) string { return item.ID })
}

func (h mediaHandler) variantContent(w http.ResponseWriter, r *http.Request) {
	content, err := h.service.OpenVariant(r.Context(), currentSession(r), r.PathValue("media_id"), r.PathValue("variant_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	defer content.Close()
	serveMedia(w, r, content)
}

func (h mediaHandler) original(w http.ResponseWriter, r *http.Request) {
	content, err := h.service.OpenOriginal(r.Context(), currentSession(r), r.PathValue("media_id"))
	if err != nil {
		requestError(w, err)
		return
	}
	defer content.Close()
	serveMedia(w, r, content)
}

var singleRangePattern = regexp.MustCompile(`^bytes=([0-9]+-[0-9]*|-[0-9]+)$`)

// Keep native conditional ordering and numeric range handling. Invalid/multipart
// ranges become a native parse error only if If-Range/preconditions permit Range.
// A mismatched If-Range correctly returns the full representation instead of 416.
func contentRequest(r *http.Request) *http.Request {
	if r.Method == "HEAD" {
		copy := r.Clone(r.Context())
		copy.Header.Del("Range")
		return copy
	}
	value := strings.TrimSpace(r.Header.Get("Range"))
	if value == "" && len(r.Header.Values("Range")) == 0 {
		return r
	}
	suffixZero := strings.HasPrefix(value, "bytes=-") && strings.TrimLeft(strings.TrimPrefix(value, "bytes=-"), "0") == ""
	if len(r.Header.Values("Range")) != 1 || !singleRangePattern.MatchString(value) || suffixZero {
		copy := r.Clone(r.Context())
		copy.Header.Set("Range", "bytes=invalid")
		return copy
	}
	if value != r.Header.Get("Range") {
		copy := r.Clone(r.Context())
		copy.Header.Set("Range", value)
		return copy
	}
	return r
}

func serveMedia(w http.ResponseWriter, r *http.Request, content *media.Content) {
	w.Header().Set("Content-Type", content.MIME)
	w.Header().Set("ETag", content.ETag)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Cookie")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox; frame-ancestors 'none'; base-uri 'none'")
	disposition := "inline"
	if content.Attachment {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": content.Filename}))
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		requestError(w, err)
		return
	}
	defer controller.SetWriteDeadline(time.Time{})
	writer := &contentWriter{ResponseWriter: w, controller: controller, ctx: r.Context(), content: content}
	// ETag is the validator. Omitting Last-Modified avoids treating sub-second
	// source overwrites as a strong HTTP-date validator for a resumed download.
	http.ServeContent(writer, contentRequest(r), content.Filename, time.Time{}, content)
	if !writer.failed && (content.Err() != nil || writer.err != nil) {
		slog.Warn("media response interrupted", "request_id", w.Header().Get("X-Request-ID"), "read_error", content.Err(), "write_error", writer.err)
		// Headers may already be sent: never turn a partial body into a silent
		// success or append Problem JSON to media bytes. Reset/close the stream.
		panic(http.ErrAbortHandler)
	}
}

type contentWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	ctx        context.Context
	content    *media.Content
	failed     bool
	err        error
}

func (writer *contentWriter) WriteHeader(status int) {
	if status < 400 {
		writer.ResponseWriter.WriteHeader(status)
		return
	}
	writer.failed = true
	// ServeContent strips private headers on errors by default. Restore our
	// boundary without requiring a process-wide GODEBUG compatibility override.
	writer.Header().Del("Content-Length")
	writer.Header().Del("Content-Disposition")
	writer.Header().Set("Cache-Control", "private, no-store")
	writer.Header().Set("ETag", writer.content.ETag)
	code, detail := "CONTENT_UNAVAILABLE", "The media response could not be completed."
	if status == 416 {
		code, detail = "INVALID_RANGE", "Send one satisfiable bytes range. Multipart ranges are not supported."
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", writer.content.Size))
	} else if status == 412 {
		code, detail = "PRECONDITION_FAILED", "The content validator no longer matches."
	} else if status >= 500 {
		status = 503
	}
	problem(writer.ResponseWriter, status, code, detail)
}

func (writer *contentWriter) Write(data []byte) (int, error) {
	if writer.failed {
		return len(data), nil // Discard ServeContent's plain-text error body.
	}
	if err := writer.ctx.Err(); err != nil {
		writer.err = err
		return 0, err
	}
	if err := writer.controller.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		writer.err = err
		return 0, err
	}
	n, err := writer.ResponseWriter.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		writer.err = err
	}
	return n, err
}
