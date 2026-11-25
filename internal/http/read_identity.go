package httpapi

import (
	"net/http"

	"gallerygo/internal/auth"
)

const (
	readUserHeader  = "X-GalleryGo-Expected-User"
	readScopeHeader = "X-GalleryGo-Expected-Scope"
)

// readIdentity restricts an already authenticated read; it never grants access.
// The echo lets a client reject a response if a proxy removed both request headers.
func readIdentity(w http.ResponseWriter, r *http.Request, session auth.Session) bool {
	users := r.Header.Values(readUserHeader)
	scopes := r.Header.Values(readScopeHeader)
	if len(users) == 0 && len(scopes) == 0 {
		return true
	}
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || len(users) != 1 || len(scopes) != 1 || !readIdentityValue(users[0]) || !readIdentityValue(scopes[0]) {
		problem(w, http.StatusBadRequest, "INVALID_READ_CONTEXT", "Provide one valid account and scope for a protected read.")
		return false
	}
	if users[0] != session.User.ID || scopes[0] != session.User.ScopeID {
		requestError(w, auth.ErrConflict)
		return false
	}
	w.Header().Set(readUserHeader, session.User.ID)
	w.Header().Set(readScopeHeader, session.User.ScopeID)
	return true
}

func readIdentityValue(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
