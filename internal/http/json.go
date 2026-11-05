package httpapi

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"gallerygo/internal/strictjson"
)

// decodeJSON rejects oversized, ambiguous, unknown-field, and non-object inputs.
// Field names are case-sensitive at every depth, following the OpenAPI contract.
func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || (parameters["charset"] != "" && !strings.EqualFold(parameters["charset"], "utf-8")) {
		problem(w, 415, "UNSUPPORTED_MEDIA_TYPE", "Send application/json with UTF-8 encoding.")
		return false
	}
	body := http.MaxBytesReader(w, r.Body, 16<<10)
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		var sizeError *http.MaxBytesError
		if errors.As(err, &sizeError) {
			problem(w, 413, "BODY_TOO_LARGE", "The JSON body exceeds 16 KiB.")
		} else {
			problem(w, 400, "INVALID_JSON", "The request body could not be read.")
		}
		return false
	}
	if !utf8.Valid(data) {
		problem(w, 400, "INVALID_JSON", "The JSON body must contain valid UTF-8.")
		return false
	}
	if err := strictjson.Decode(data, destination); err != nil {
		problem(w, 400, "INVALID_JSON", "Send one JSON object with unambiguous values, exact known field names and valid types.")
		return false
	}
	return true
}

func requireEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	if r.Body != nil {
		data, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil || len(data) != 0 {
			problem(w, 400, "INVALID_BODY", "This command does not accept a request body.")
			return false
		}
	}
	return true
}
