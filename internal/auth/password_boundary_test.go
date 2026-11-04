package auth

import (
	"strings"
	"testing"
)

func TestLoginRejectsBcryptTruncation(t *testing.T) {
	s, user := testService(t)
	password := strings.Repeat("a", 72)
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET password_hash=? WHERE id=?`, hash, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(t.Context(), "owner", password, "127.0.0.1", "test", ""); err != nil {
		t.Fatal(err)
	}
	_, err = s.Login(t.Context(), "owner", password+"suffix", "127.0.0.1", "test", "")
	assertError(t, err, ErrCredentials)
}
