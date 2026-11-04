// Package httpapi implements GalleryGo's HTTP contract.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
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
