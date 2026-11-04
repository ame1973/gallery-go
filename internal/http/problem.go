// Package httpapi implements GalleryGo's HTTP contract.
package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/media"
)

type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"request_id"`
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Warn("response write failed", "error", err)
	}
}

func problem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(Problem{Type: "urn:gallerygo:problem:" + code, Title: http.StatusText(status), Status: status, Code: code, Detail: detail, RequestID: w.Header().Get("X-Request-ID")}); err != nil {
		slog.Warn("problem response write failed", "error", err)
	}
}

func requestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrCredentials):
		problem(w, 401, "INVALID_CREDENTIALS", "The username or password is incorrect.")
	case errors.Is(err, auth.ErrUnauthenticated):
		problem(w, 401, "UNAUTHENTICATED", "Sign in to continue.")
	case errors.Is(err, auth.ErrForbidden):
		problem(w, 403, "ADMIN_REQUIRED", "Administrator access is required.")
	case errors.Is(err, auth.ErrLastAdmin):
		problem(w, 409, "LAST_ACTIVE_ADMIN", "Keep at least one active administrator before making this change.")
	case errors.Is(err, auth.ErrUsernameTaken):
		problem(w, 409, "USERNAME_TAKEN", "Choose a different username.")
	case errors.Is(err, access.ErrNameTaken):
		problem(w, 409, "GROUP_NAME_TAKEN", "Choose a different permission group name.")
	case errors.Is(err, access.ErrLibraryNotEmpty):
		problem(w, 409, "LIBRARY_NOT_EMPTY", "Deactivate the library instead; its sources and albums must be preserved.")
	case errors.Is(err, auth.ErrIdempotencyReuse):
		problem(w, 409, "IDEMPOTENCY_CONFLICT", "Use the original request body with this key, or a new key for a new request.")
	case errors.Is(err, auth.ErrRequestInFlight):
		w.Header().Set("Retry-After", "1")
		problem(w, 409, "IDEMPOTENCY_IN_PROGRESS", "Retry the same request with the same idempotency key.")
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrUsername), errors.Is(err, auth.ErrInvalidUser), errors.Is(err, auth.ErrIdempotencyKey), errors.Is(err, access.ErrName), errors.Is(err, access.ErrGroupSet), errors.Is(err, access.ErrUnknownGroup), errors.Is(err, access.ErrRestriction):
		problem(w, 422, "VALIDATION_FAILED", err.Error())
	case errors.Is(err, auth.ErrRateLimited):
		w.Header().Set("Retry-After", "900")
		problem(w, 429, "RATE_LIMITED", "Too many authentication attempts. Try again later.")
	case errors.Is(err, auth.ErrConflict):
		problem(w, 409, "VERSION_CONFLICT", "The resource changed while the request was being processed.")
	case errors.Is(err, media.ErrSourceUnavailable), errors.Is(err, media.ErrVariantUnavailable), errors.Is(err, media.ErrContentChanged):
		slog.Warn("media content unavailable", "request_id", w.Header().Get("X-Request-ID"), "error", err)
		w.Header().Set("Retry-After", "30")
		code := "VARIANT_UNAVAILABLE"
		if errors.Is(err, media.ErrSourceUnavailable) {
			code = "SOURCE_UNAVAILABLE"
		}
		problem(w, 503, code, "The requested media bytes are not currently available.")
	case errors.Is(err, sql.ErrNoRows):
		problem(w, 404, "NOT_FOUND", "The resource does not exist.")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		problem(w, 503, "REQUEST_TIMEOUT", "The request could not finish in time.")
	default:
		slog.Error("API request failed", "request_id", w.Header().Get("X-Request-ID"), "error", err)
		problem(w, 500, "INTERNAL_ERROR", "The request could not be completed.")
	}
}
