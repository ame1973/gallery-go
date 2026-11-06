package httpapi

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gallerygo/internal/media"
)

// time.Parse accepts some non-RFC3339 spellings (comma fractions/offset 24:00).
// Bound the exact wire grammar before interpreting the explicit instant.
var captureBoundPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T(?:[01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](?:\.[0-9]{1,9})?(?:Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])$`)

func albumMediaPage(w http.ResponseWriter, r *http.Request) (pagination, media.AlbumMediaFilter, bool) {
	page, ok := readPage(w, r)
	if !ok {
		return page, media.AlbumMediaFilter{}, false
	}
	if page.query.Has("cursor") && page.query.Get("cursor") == "" {
		problem(w, 400, "INVALID_CURSOR", "Omit the cursor for a first page; a supplied continuation must not be empty.")
		return page, media.AlbumMediaFilter{}, false
	}
	for key, values := range page.query {
		if key == "limit" || key == "cursor" {
			continue
		}
		if (key != "kind" && key != "captured_from" && key != "captured_before" && key != "sort") || len(values) != 1 || values[0] == "" {
			problem(w, 400, "INVALID_FILTER", "Use one nonempty kind, captured_from, captured_before or sort value, with limit and cursor.")
			return page, media.AlbumMediaFilter{}, false
		}
	}
	filter := media.AlbumMediaFilter{Kind: page.query.Get("kind"), Sort: page.query.Get("sort")}
	for key, destination := range map[string]**time.Time{"captured_from": &filter.CapturedFrom, "captured_before": &filter.CapturedBefore} {
		if value := page.query.Get(key); value != "" {
			instant, err := time.Parse(time.RFC3339Nano, value)
			if err != nil || !captureBoundPattern.MatchString(value) || strings.HasSuffix(value, "-00:00") {
				problem(w, 400, "INVALID_FILTER", "Capture bounds require RFC3339 instants with an explicit known offset and millisecond precision.")
				return page, media.AlbumMediaFilter{}, false
			}
			*destination = &instant
		}
	}
	if err := filter.Validate(); err != nil {
		requestError(w, err)
		return page, filter, false
	}
	return page, filter, true
}

func (h mediaHandler) albumMedia(w http.ResponseWriter, r *http.Request) {
	page, filter, ok := albumMediaPage(w, r)
	if !ok {
		return
	}
	position := media.AlbumMediaPosition{ID: page.cursor.After, Capture: page.cursor.Position}
	items, err := h.service.AlbumMedia(r.Context(), currentSession(r), r.PathValue("album_id"), filter, position, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	if len(items) > page.limit && strings.HasPrefix(filter.Sort, "captured_") {
		page.cursor.Position = "unknown"
		if instant := items[page.limit-1].CaptureAt; instant != nil {
			page.cursor.Position = strconv.FormatInt(instant.UnixMilli(), 10)
		}
	}
	writePage(w, page, items, func(item media.Metadata) string { return item.ID })
}
