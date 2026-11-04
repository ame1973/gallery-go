// Package auth owns credentials, sessions, and authentication abuse controls.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const passwordCost = 12

var (
	ErrCredentials     = errors.New("invalid credentials")
	ErrUnauthenticated = errors.New("a valid session is required")
	ErrWeakPassword    = errors.New("password must contain at least 15 characters and at most 72 UTF-8 bytes")
	ErrUsername        = errors.New("username must contain 3 to 64 ASCII letters, digits, dots, underscores, or hyphens")
	ErrBootstrapped    = errors.New("administrator bootstrap is already complete")
	ErrRateLimited     = errors.New("too many authentication attempts")
	ErrConflict        = errors.New("the account changed; retry with the current session")
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

func NormalizeUsername(username string) (string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(username) {
		return "", ErrUsername
	}
	return username, nil
}

func validatePassword(password string) error {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < 15 || len(password) > 72 {
		return ErrWeakPassword
	}
	return nil
}

func HashPassword(password string) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), passwordCost)
	return string(hash), err
}

func token() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// The CSRF token is stable for a session but unavailable to JavaScript until an
// authenticated same-origin response returns it. Only its hash is stored in SQLite.
func csrfToken(sessionToken string) string {
	mac := hmac.New(sha256.New, []byte(sessionToken))
	mac.Write([]byte("gallerygo:v2:csrf"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validToken(value string) bool {
	if len(value) != 43 {
		return false
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(data) == 32
}
