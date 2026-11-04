package httpapi

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"

	"gallerygo/internal/auth"
)

const sessionCookie = "__Host-gallerygo_session"

type sessionContextKey struct{}

type authentication struct {
	service *auth.Service
	origin  string
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
type passwordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}
type csrfResponse struct {
	Token string `json:"token"`
}
type sessionPage = listPage[auth.Session]

func (a authentication) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := a.service.Authenticate(r.Context(), cookieToken(r))
		if err != nil {
			requestError(w, err)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if !a.sameOrigin(w, r) {
				return
			}
			if !session.ValidCSRF(r.Header.Get("X-CSRF-Token")) {
				problem(w, 403, "CSRF_REJECTED", "Refresh the session security token and try again.")
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, session)))
	}
}

func currentSession(r *http.Request) auth.Session {
	return r.Context().Value(sessionContextKey{}).(auth.Session)
}

func cookieToken(r *http.Request) string {
	cookies := r.CookiesNamed(sessionCookie)
	if len(cookies) != 1 {
		return ""
	}
	return cookies[0].Value
}

func (a authentication) sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Origin") != a.origin || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		problem(w, 403, "ORIGIN_REJECTED", "A same-origin request is required.")
		return false
	}
	return true
}

func setSessionCookie(w http.ResponseWriter, session auth.Session) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: session.Token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt, MaxAge: max(1, int(time.Until(session.ExpiresAt).Seconds()))})
}

func expireSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: time.Unix(1, 0).UTC(), MaxAge: -1})
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "unknown"
	}
	return ip.String()
}

func (a authentication) login(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(w, r) {
		return
	}
	var input loginRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	if len(input.Username) > 64 || input.Username == "" || input.Password == "" || len(input.Password) > 72 {
		problem(w, 422, "VALIDATION_FAILED", "A username and password within the documented length limits are required.")
		return
	}
	userAgent := r.UserAgent()
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	userAgent = strings.ToValidUTF8(userAgent, "")
	session, err := a.service.Login(r.Context(), input.Username, input.Password, peerIP(r), userAgent, cookieToken(r))
	if err != nil {
		requestError(w, err)
		return
	}
	setSessionCookie(w, session)
	writeJSON(w, 200, session.User)
}

func (a authentication) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, currentSession(r).User)
}
func (a authentication) csrf(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, csrfResponse{currentSession(r).CSRFToken()})
}

func (a authentication) logout(w http.ResponseWriter, r *http.Request) {
	session := currentSession(r)
	if err := a.service.Revoke(r.Context(), session, session.ID); err != nil {
		requestError(w, err)
		return
	}
	expireSessionCookie(w)
	w.WriteHeader(204)
}

func (a authentication) password(w http.ResponseWriter, r *http.Request) {
	var input passwordRequest
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.OldPassword == "" || len(input.OldPassword) > 72 {
		problem(w, 422, "VALIDATION_FAILED", "The current password is required and must not exceed 72 UTF-8 bytes.")
		return
	}
	session, err := a.service.ChangePassword(r.Context(), currentSession(r), input.OldPassword, input.NewPassword)
	if err != nil {
		requestError(w, err)
		return
	}
	setSessionCookie(w, session)
	writeJSON(w, 200, session.User)
}

func (a authentication) sessions(w http.ResponseWriter, r *http.Request) {
	page, ok := readPage(w, r)
	if !ok {
		return
	}
	items, err := a.service.Sessions(r.Context(), currentSession(r), page.cursor.After, page.limit+1)
	if err != nil {
		requestError(w, err)
		return
	}
	writePage(w, page, items, func(session auth.Session) string { return session.ID })
}

func (a authentication) revoke(w http.ResponseWriter, r *http.Request) {
	session := currentSession(r)
	id := r.PathValue("session_id")
	if err := a.service.Revoke(r.Context(), session, id); err != nil {
		requestError(w, err)
		return
	}
	if id == session.ID {
		expireSessionCookie(w)
	}
	w.WriteHeader(204)
}
