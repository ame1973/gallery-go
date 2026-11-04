package httpapi

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"gallerygo/internal/auth"
)

func (a authentication) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentSession(r).User.Role != "admin" {
			authError(w, auth.ErrForbidden)
			return
		}
		next(w, r)
	}
}

func userETag(user auth.User) string { return fmt.Sprintf(`"user:%s:%d"`, user.ID, user.Version) }

func userPrecondition(w http.ResponseWriter, r *http.Request) (int64, bool) {
	values := r.Header.Values("If-Match")
	if len(values) == 0 {
		problem(w, 428, "PRECONDITION_REQUIRED", "Send the account's ETag in If-Match.")
		return 0, false
	}
	id := r.PathValue("user_id")
	value := r.Header.Get("If-Match")
	version, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(value, `"user:`+id+`:`), `"`), 10, 64)
	if len(values) != 1 || err != nil || version < 1 || value != userETag(auth.User{ID: id, Version: version}) {
		problem(w, 400, "INVALID_PRECONDITION", "Send one strong account ETag, without wildcards or lists.")
		return 0, false
	}
	return version, true
}

func (a authentication) users(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	items, err := a.service.Users(r.Context(), currentSession(r), page.cursor.After, page.limit+1)
	if err != nil {
		authError(w, err)
		return
	}
	writePage(w, page, items, func(user auth.User) string { return user.ID })
}

func (a authentication) user(w http.ResponseWriter, r *http.Request) {
	user, err := a.service.GetUser(r.Context(), currentSession(r), r.PathValue("user_id"))
	if err != nil {
		authError(w, err)
		return
	}
	w.Header().Set("ETag", userETag(user))
	writeJSON(w, 200, user)
}

type createUserRequest struct {
	Username         string  `json:"username"`
	Password         string  `json:"password"`
	Role             *string `json:"role"`
	Active           *bool   `json:"active"`
	DownloadOriginal bool    `json:"download_original"`
}

func (a authentication) createUser(w http.ResponseWriter, r *http.Request) {
	var input createUserRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	if len(input.Username) > 64 {
		authError(w, auth.ErrUsername)
		return
	}
	role := "user"
	if input.Role != nil {
		role = *input.Role
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	user, err := a.service.CreateUser(r.Context(), currentSession(r), auth.NewUser{
		Username: input.Username, Password: input.Password, Role: role,
		Active: input.Active == nil || *input.Active, DownloadOriginal: input.DownloadOriginal,
	}, key)
	if err != nil {
		authError(w, err)
		return
	}
	w.Header().Set("ETag", userETag(user))
	w.Header().Set("Location", "/api/v2/admin/users/"+user.ID)
	writeJSON(w, 201, user)
}

func (a authentication) updateUser(w http.ResponseWriter, r *http.Request) {
	version, ok := userPrecondition(w, r)
	if !ok {
		return
	}
	var input auth.UserPatch
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Username != nil && len(*input.Username) > 64 {
		authError(w, auth.ErrUsername)
		return
	}
	current := currentSession(r)
	user, err := a.service.UpdateUser(r.Context(), current, r.PathValue("user_id"), version, input)
	if err != nil {
		authError(w, err)
		return
	}
	if user.ID == current.User.ID && user.ScopeID != current.User.ScopeID {
		expireSessionCookie(w)
	}
	w.Header().Set("ETag", userETag(user))
	writeJSON(w, 200, user)
}

func (a authentication) deleteUser(w http.ResponseWriter, r *http.Request) {
	version, ok := userPrecondition(w, r)
	if !ok {
		return
	}
	current := currentSession(r)
	id := r.PathValue("user_id")
	if err := a.service.DeleteUser(r.Context(), current, id, version); err != nil {
		authError(w, err)
		return
	}
	if id == current.User.ID {
		expireSessionCookie(w)
	}
	w.WriteHeader(204)
}

func (a authentication) resetPassword(w http.ResponseWriter, r *http.Request) {
	version, ok := userPrecondition(w, r)
	if !ok {
		return
	}
	var input struct {
		NewPassword string `json:"new_password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	current := currentSession(r)
	user, err := a.service.ResetPassword(r.Context(), current, r.PathValue("user_id"), version, input.NewPassword, key)
	if err != nil {
		authError(w, err)
		return
	}
	if user.ID == current.User.ID && user.Version > current.User.Version {
		expireSessionCookie(w)
	}
	w.Header().Set("ETag", userETag(user))
	writeJSON(w, 200, user)
}

func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	if len(r.Header.Values("Idempotency-Key")) != 1 {
		authError(w, auth.ErrIdempotencyKey)
		return "", false
	}
	return r.Header.Get("Idempotency-Key"), true
}

func (a authentication) userSessions(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	items, err := a.service.UserSessions(r.Context(), currentSession(r), r.PathValue("user_id"), page.cursor.After, page.limit+1)
	if err != nil {
		authError(w, err)
		return
	}
	writePage(w, page, items, func(session auth.Session) string { return session.ID })
}

func (a authentication) revokeUserSession(w http.ResponseWriter, r *http.Request) {
	current := currentSession(r)
	id := r.PathValue("session_id")
	if err := a.service.RevokeUserSession(r.Context(), current, r.PathValue("user_id"), id); err != nil {
		authError(w, err)
		return
	}
	if id == current.ID {
		expireSessionCookie(w)
	}
	w.WriteHeader(204)
}

func (a authentication) audit(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	before := int64(math.MaxInt64)
	if page.cursor.After != "" {
		var err error
		before, err = strconv.ParseInt(page.cursor.After, 10, 64)
		if err != nil || before < 1 {
			problem(w, 400, "INVALID_CURSOR", "The audit cursor is invalid.")
			return
		}
	}
	items, err := a.service.Audit(r.Context(), currentSession(r), before, page.limit+1)
	if err != nil {
		authError(w, err)
		return
	}
	writePage(w, page, items, func(event auth.AuditEvent) string { return event.ID })
}
