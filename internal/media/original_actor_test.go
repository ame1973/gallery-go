package media

import (
	"database/sql"
	"testing"
)

func TestOriginalContentPinsAuthenticatedAccount(t *testing.T) {
	f := fixture(t)
	state, err := f.svc.originalContentState(t.Context(), f.reader, f.mediaID)
	if err != nil {
		t.Fatal(err)
	}
	// Model a stale request if the persisted session's account changes after the
	// first state read; the normal application does not reassign session owners.
	if _, err := f.db.Exec(`UPDATE sessions SET user_id=? WHERE id=?`, f.owner.User.ID, f.reader.ID); err != nil {
		t.Fatal(err)
	}
	requireError(t, f.svc.requireOriginalContentState(t.Context(), f.reader, f.mediaID, state), sql.ErrNoRows)
	content, err := f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	if content != nil {
		content.Close()
	}
	requireError(t, err, sql.ErrNoRows)
}

func TestOriginalContentRequiresExpectedRevision(t *testing.T) {
	f := fixture(t)
	for _, revision := range []string{"", "old-revision"} {
		content, err := f.svc.OpenOriginalForRevision(t.Context(), f.reader, f.mediaID, revision)
		if content != nil {
			content.Close()
		}
		requireError(t, err, sql.ErrNoRows)
	}
	content, err := f.svc.OpenOriginalForRevision(t.Context(), f.reader, f.mediaID, "revision")
	if err != nil {
		t.Fatal(err)
	}
	content.Close()
}
