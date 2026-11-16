package access

import (
	"errors"
	"testing"

	"gallerygo/internal/auth"
)

func TestAccessFixtureDatabasesAndSessionsAreIsolated(t *testing.T) {
	first, firstOwner := fixture(t)
	second, secondOwner := fixture(t)
	if first.db == second.db {
		t.Fatal("fixtures share a database handle")
	}
	// The image deliberately shares a test-only owner, not any session.
	if firstOwner.User.ID != secondOwner.User.ID || firstOwner.User.ScopeID != secondOwner.User.ScopeID {
		t.Fatal("fixtures did not use the expected bootstrap image")
	}
	if firstOwner.Token == secondOwner.Token || firstOwner.ID == secondOwner.ID {
		t.Fatal("separate fixture logins shared a session")
	}
	if _, err := first.auth.Authenticate(t.Context(), secondOwner.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("first database accepted the second session", err)
	}
	if _, err := second.auth.Authenticate(t.Context(), firstOwner.Token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("second database accepted the first session", err)
	}
	group(t, first, firstOwner, "First fixture only")
	var firstCount, secondCount int
	if err := first.db.QueryRow(`SELECT COUNT(*) FROM permission_groups`).Scan(&firstCount); err != nil {
		t.Fatal(err)
	}
	if err := second.db.QueryRow(`SELECT COUNT(*) FROM permission_groups`).Scan(&secondCount); err != nil {
		t.Fatal(err)
	}
	if firstCount != 1 || secondCount != 0 {
		t.Fatal("fixture mutations crossed database boundaries", firstCount, secondCount)
	}
}
