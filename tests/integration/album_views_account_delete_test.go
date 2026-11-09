package integration

import (
	"encoding/json"
	"testing"

	"gallerygo/internal/store"
)

func TestAlbumViewsAccountDeletionRefreshesHiddenTotalsAtomically(t *testing.T) {
	f := newCollectionFixture(t)
	seedAlbumViews(t, f)
	ctx := t.Context()
	for _, input := range []struct {
		album string
		owner bool
	}{
		{"view-a", false}, {"view-a", false}, {"view-b", false}, {"view-a", true},
	} {
		actor := f.other
		if input.owner {
			actor = f.owner
		}
		if _, err := f.service.RecordAlbumView(ctx, actor, input.album, store.NewID()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`UPDATE albums SET access_restricted=1 WHERE id='view-b'`); err != nil {
		t.Fatal(err)
	}
	counts := func() [6]int64 {
		t.Helper()
		var got [6]int64
		if err := f.db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM users WHERE id=?),
 (SELECT COUNT(*) FROM album_views WHERE user_id=?),
 (SELECT view_count FROM albums WHERE id='view-a'),(SELECT view_count FROM albums WHERE id='view-b'),
 (SELECT version FROM albums WHERE id='view-a'),(SELECT version FROM albums WHERE id='view-b')`, f.other.User.ID, f.other.User.ID).Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5]); err != nil {
			t.Fatal(err)
		}
		return got
	}
	before, events := counts(), albumViewState(t, f)
	if before != [6]int64{1, 2, 3, 1, 4, 2} {
		t.Fatal("account deletion fixture lost view totals", before)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_hidden_view_delete BEFORE INSERT ON change_log WHEN NEW.kind='album' AND NEW.resource_id='view-b' BEGIN SELECT RAISE(ABORT,'injected hidden view journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.DeleteUser(ctx, f.owner, f.other.User.ID, f.other.User.Version); err == nil {
		t.Fatal("late hidden-view journal failure was ignored")
	}
	if after := counts(); after != before || albumViewState(t, f) != events {
		t.Fatal("late deletion failure leaked user, view, total or event changes", before, after)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_hidden_view_delete`); err != nil {
		t.Fatal(err)
	}
	if err := f.auth.DeleteUser(ctx, f.owner, f.other.User.ID, f.other.User.Version); err != nil {
		t.Fatal(err)
	}
	if got := counts(); got != [6]int64{0, 0, 1, 0, 5, 3} {
		t.Fatal("deleted account's hidden views or shared totals were retained", got)
	}
	var lastPayload, scope string
	if err := f.db.QueryRow(`SELECT payload_json,actor_scope FROM change_log WHERE kind='album' AND resource_id='view-b' ORDER BY seq DESC LIMIT 1`).Scan(&lastPayload, &scope); err != nil || scope != "system" {
		t.Fatal("hidden album aggregate was not published in the canonical shared stream", lastPayload, scope, err)
	}
	var projection struct {
		ViewCount int64 `json:"view_count"`
		Version   int64 `json:"version"`
	}
	if err := json.Unmarshal([]byte(lastPayload), &projection); err != nil || projection.ViewCount != 0 || projection.Version != 3 {
		t.Fatal("hidden album event did not contain its updated total and revision", lastPayload, err)
	}
	var missing int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE c.kind IN ('album','album_view') AND c.resource_id IN ('view-a','view-b') AND o.id IS NULL`).Scan(&missing); err != nil || missing != 0 {
		t.Fatal("account deletion left a shared or private event without an outbox", missing, err)
	}
}
