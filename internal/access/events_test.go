package access

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestGroupPolicyOutboxFailureRestoresEarlierScopeWrites(t *testing.T) {
	s, owner := fixture(t)
	g, lib := group(t, s, owner, "Family"), library(t, s, owner, "Library")
	viewer := reader(t, s, "reader")
	membership(t, s, owner, viewer, g.ID)
	if _, err := s.SetLibraryPolicy(t.Context(), owner, lib.ID, 1, []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	id := album(t, s, lib.ID, "Private")
	if _, err := s.SetAlbumPolicy(t.Context(), owner, id, 1, true, []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	before, err := s.UserGroups(t.Context(), owner, viewer.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	var events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM change_log`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_policy_outbox BEFORE INSERT ON outbox_events WHEN NEW.id LIKE 'policy:%' BEGIN SELECT RAISE(ABORT,'injected policy outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGroup(t.Context(), owner, g.ID, 1); err == nil {
		t.Fatal("group deletion committed without all policy outbox rows")
	}
	after, err := s.UserGroups(t.Context(), owner, viewer.User.ID)
	if err != nil || after.ScopeID != before.ScopeID || after.Version != before.Version || !slices.Equal(after.GroupIDs, before.GroupIDs) {
		t.Fatal("scope/membership writes were not rolled back", after, err)
	}
	var actual int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM change_log`).Scan(&actual); err != nil || actual != events {
		t.Fatal("partial event journal survived rollback", actual, err)
	}
	policy, err := s.AlbumPolicy(t.Context(), owner, id)
	if err != nil || !policy.Restricted || policy.Version != 2 || !slices.Equal(policy.GroupIDs, []string{g.ID}) {
		t.Fatal("policy did not survive failed deletion", policy, err)
	}
}

func TestGroupDeletionJournalsEveryChangedPolicyVersion(t *testing.T) {
	s, owner := fixture(t)
	removed, retained, lib := group(t, s, owner, "Removed"), group(t, s, owner, "Retained"), library(t, s, owner, "Library")
	if _, err := s.SetLibraryPolicy(t.Context(), owner, lib.ID, 1, []string{removed.ID, retained.ID}); err != nil {
		t.Fatal(err)
	}
	id := album(t, s, lib.ID, "Restricted")
	if _, err := s.SetAlbumPolicy(t.Context(), owner, id, 1, true, []string{removed.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGroup(t.Context(), owner, removed.ID, 1); err != nil {
		t.Fatal(err)
	}
	var libraryData, albumData string
	if err := s.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='library_access' AND resource_id=? ORDER BY seq DESC LIMIT 1`, lib.ID).Scan(&libraryData); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='album_access' AND resource_id=? ORDER BY seq DESC LIMIT 1`, id).Scan(&albumData); err != nil {
		t.Fatal(err)
	}
	var lp LibraryPolicy
	var ap AlbumPolicy
	if err := json.Unmarshal([]byte(libraryData), &lp); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(albumData), &ap); err != nil {
		t.Fatal(err)
	}
	if lp.Version != 3 || !slices.Equal(lp.GroupIDs, []string{retained.ID}) {
		t.Fatalf("library policy journal is stale: %+v", lp)
	}
	if ap.Version != 3 || !ap.Restricted || ap.GroupIDs == nil || len(ap.GroupIDs) != 0 {
		t.Fatalf("album policy journal is stale: %+v", ap)
	}
	var missing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE o.id IS NULL`).Scan(&missing); err != nil || missing != 0 {
		t.Fatal("policy changes have no outbox row", missing, err)
	}
}
