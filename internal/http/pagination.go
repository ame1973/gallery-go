package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

type pageCursor struct {
	Scope      string `json:"scope"`
	User       string `json:"user"`
	Collection string `json:"collection"`
	After      string `json:"after"`
}

type pagination struct {
	limit  int
	cursor pageCursor
}

type listPage[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
	HasMore    bool    `json:"has_more"`
}

func readPage(w http.ResponseWriter, r *http.Request) (pagination, bool) {
	current := currentSession(r)
	page := pagination{limit: 30, cursor: pageCursor{Scope: current.User.ScopeID, User: current.User.ID, Collection: r.URL.Path}}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["limit"]) > 1 || len(query["cursor"]) > 1 {
		problem(w, 400, "INVALID_PAGINATION", "Use one limit and one cursor with valid query encoding.")
		return page, false
	}
	if query.Has("limit") {
		page.limit, err = strconv.Atoi(query.Get("limit"))
		if err != nil || page.limit < 1 || page.limit > 100 {
			problem(w, 400, "INVALID_PAGINATION", "Limit must be between 1 and 100.")
			return page, false
		}
	}
	if encoded := query.Get("cursor"); encoded != "" {
		if len(encoded) > 512 {
			problem(w, 400, "INVALID_CURSOR", "The page cursor is invalid.")
			return page, false
		}
		var cursor pageCursor
		data, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Scope != page.cursor.Scope || cursor.User != page.cursor.User || cursor.Collection != page.cursor.Collection || len(cursor.After) == 0 || len(cursor.After) > 64 {
			problem(w, 400, "INVALID_CURSOR", "The page cursor is invalid for this account or collection.")
			return page, false
		}
		page.cursor = cursor
	}
	return page, true
}

func writePage[T any](w http.ResponseWriter, page pagination, items []T, id func(T) string) {
	result := listPage[T]{Items: items, HasMore: len(items) > page.limit}
	if result.HasMore {
		result.Items = items[:page.limit]
		page.cursor.After = id(result.Items[page.limit-1])
		data, _ := json.Marshal(page.cursor) // Only strings; encoding cannot fail.
		next := base64.RawURLEncoding.EncodeToString(data)
		result.NextCursor = &next
	}
	writeJSON(w, 200, result)
}
