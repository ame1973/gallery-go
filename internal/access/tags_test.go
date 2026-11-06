package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func tagCounts(t *testing.T, s *Service) [4]int64 {
	t.Helper()
	var counts [4]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log WHERE kind='tag'),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events WHERE resource_type='tag'),(SELECT COUNT(*) FROM mutation_receipts WHERE operation LIKE 'tag.%')`).Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestTagCatalogCommandsKeepExactLabelsVersionsAndHistoricalReceipts(t *testing.T) {
	s, owner := fixture(t)
	counts := tagCounts(t, s)
	createKey := store.NewID()
	created, err := s.CreateTag(t.Context(), owner, "  旅行  ", createKey)
	if err != nil || created.ID == "" || created.Name != "旅行" || created.Version != 1 {
		t.Fatal("tag creation lost exact trimmed label/version", created, err)
	}
	afterCreate := tagCounts(t, s)
	for index := range counts {
		if afterCreate[index] != counts[index]+1 {
			t.Fatal("tag row/change/outbox/audit/receipt did not co-commit", counts, afterCreate)
		}
	}
	replay, err := s.CreateTag(t.Context(), owner, "旅行", createKey)
	if err != nil || replay != created || tagCounts(t, s) != afterCreate {
		t.Fatal("canonical create receipt duplicated or changed the tag", replay, err)
	}
	if _, err := s.CreateTag(t.Context(), owner, "旅行", store.NewID()); !errors.Is(err, ErrTagNameTaken) {
		t.Fatal("a new command accepted the same exact label", err)
	}
	if _, err := s.CreateTag(t.Context(), owner, "Different", createKey); !errors.Is(err, store.ErrIdempotencyReuse) {
		t.Fatal("creation key was reused for another label", err)
	}
	for _, name := range []string{"Travel", "travel", "Ｔravel"} {
		if value, err := s.CreateTag(t.Context(), owner, name, store.NewID()); err != nil || value.Name != name || value.ID == created.ID {
			t.Fatal("catalog identity was silently case-folded or NFKC-normalized", value, err)
		}
	}
	renameKey := store.NewID()
	changed, err := s.RenameTag(t.Context(), owner, created.ID, 1, "  New label  ", renameKey)
	if err != nil || changed.ID != created.ID || changed.Name != "New label" || changed.Version != 2 {
		t.Fatal("tag rename lost logical identity or strong version", changed, err)
	}
	beforeNoop := tagCounts(t, s)
	noop, err := s.RenameTag(t.Context(), owner, created.ID, 2, "New label", store.NewID())
	if err != nil || noop != changed {
		t.Fatal("identical label was not a version no-op", noop, err)
	}
	afterNoop := tagCounts(t, s)
	if afterNoop[0] != beforeNoop[0] || afterNoop[1] != beforeNoop[1] || afterNoop[2] != beforeNoop[2] || afterNoop[3] != beforeNoop[3]+1 {
		t.Fatal("no-op wrote business events or omitted its receipt", beforeNoop, afterNoop)
	}
	if _, err := s.RenameTag(t.Context(), owner, created.ID, 1, "Fresh stale", store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("new stale rename ignored its version", err)
	}
	if _, err := s.RenameTag(t.Context(), owner, created.ID, 2, "Travel", store.NewID()); !errors.Is(err, ErrTagNameTaken) {
		t.Fatal("rename accepted another exact catalog label", err)
	}
	if _, err := s.RenameTag(t.Context(), owner, created.ID, 2, "New label", renameKey); !errors.Is(err, store.ErrIdempotencyReuse) {
		t.Fatal("historical rename key accepted another precondition", err)
	}
	deleteKey := store.NewID()
	if err := s.DeleteTag(t.Context(), owner, created.ID, 2, deleteKey); err != nil {
		t.Fatal(err)
	}
	afterDelete := tagCounts(t, s)
	for index := range afterNoop {
		if afterDelete[index] != afterNoop[index]+1 {
			t.Fatal("tag deletion did not atomically tombstone/audit/receipt", afterNoop, afterDelete)
		}
	}
	if err := s.DeleteTag(t.Context(), owner, created.ID, 2, deleteKey); err != nil || tagCounts(t, s) != afterDelete {
		t.Fatal("deleted-resource receipt did not replay without side effects", err)
	}
	if err := s.DeleteTag(t.Context(), owner, created.ID, 2, store.NewID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("a new delete command resurrected or retargeted the tag", err)
	}
	if err := s.DeleteTag(t.Context(), owner, created.ID, 1, deleteKey); !errors.Is(err, store.ErrIdempotencyReuse) {
		t.Fatal("deletion key accepted another version", err)
	}
	replay, err = s.CreateTag(t.Context(), owner, "旅行", createKey)
	if err != nil || replay != created {
		t.Fatal("create receipt lost its historical result after rename/deletion", replay, err)
	}
	replay, err = s.RenameTag(t.Context(), owner, created.ID, 1, "New label", renameKey)
	if err != nil || replay != changed {
		t.Fatal("rename receipt lost its historical result after deletion", replay, err)
	}
	if _, err := s.Tag(t.Context(), owner, created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("historical receipt was mistaken for current catalog existence", err)
	}
	var op, payload string
	var version int64
	if err := s.db.QueryRow(`SELECT operation,version,payload_json FROM change_log WHERE kind='tag' AND resource_id=? ORDER BY seq DESC LIMIT 1`, created.ID).Scan(&op, &version, &payload); err != nil || op != "delete" || version != 3 || !strings.Contains(payload, created.ID) {
		t.Fatal("tag deletion did not retain its monotonic tombstone", op, version, payload, err)
	}
}

func TestTagCatalogAssignedLabelsCannotCascadeOrChangeAlbumPolicy(t *testing.T) {
	s, owner := fixture(t)
	tag, err := s.CreateTag(t.Context(), owner, "Assigned", store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,is_active,created_at) VALUES('hidden-library','Hidden',0,0);
 INSERT INTO albums(id,library_id,source_name,access_restricted,version,created_at,updated_at) VALUES('hidden-album','hidden-library','Preserved',1,7,0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO album_tags(album_id,tag_id) VALUES('hidden-album',?)`, tag.ID); err != nil {
		t.Fatal(err)
	}
	counts := tagCounts(t, s)
	if err := s.DeleteTag(t.Context(), owner, tag.ID, 1, store.NewID()); !errors.Is(err, ErrTagInUse) {
		t.Fatal("assigned tag deletion ignored a hidden/inactive album edge", err)
	}
	if tagCounts(t, s) != counts {
		t.Fatal("rejected deletion wrote side effects")
	}
	changed, err := s.RenameTag(t.Context(), owner, tag.ID, 1, "Renamed", store.NewID())
	if err != nil || changed.Version != 2 {
		t.Fatal("assigned tag label could not be versioned independently", changed, err)
	}
	var links, albumVersion int
	var restricted bool
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_tags WHERE tag_id=?),version,access_restricted FROM albums WHERE id='hidden-album'`, tag.ID).Scan(&links, &albumVersion, &restricted); err != nil || links != 1 || albumVersion != 7 || !restricted {
		t.Fatal("catalog rename removed an association or changed album policy/version", links, albumVersion, restricted, err)
	}
}

func TestTagCatalogBoundsFreshAuthorizationAndVersionOverflow(t *testing.T) {
	s, owner := fixture(t)
	for _, name := range []string{"", "  ", "bad\x00label", "bad\xff", strings.Repeat("字", 101)} {
		if _, err := s.CreateTag(t.Context(), owner, name, store.NewID()); !errors.Is(err, ErrName) {
			t.Fatal("invalid tag label was accepted", name, err)
		}
	}
	key := store.NewID()
	tag, err := s.CreateTag(t.Context(), owner, "Private unused label", key)
	if err != nil {
		t.Fatal(err)
	}
	viewer := reader(t, s, "tag-reader")
	forged := viewer
	forged.User.Role = "admin"
	for _, session := range []auth.Session{viewer, forged} {
		if _, err := s.Tags(t.Context(), session, "", 30); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role exposed unused/global private tag names", err)
		}
		if _, err := s.Tag(t.Context(), session, tag.ID); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role exposed a private tag", err)
		}
		if _, err := s.CreateTag(t.Context(), session, "Denied", store.NewID()); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role created a tag", err)
		}
		if _, err := s.RenameTag(t.Context(), session, tag.ID, 1, "Denied", store.NewID()); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role renamed a tag", err)
		}
		if err := s.DeleteTag(t.Context(), session, tag.ID, 1, store.NewID()); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role deleted a tag", err)
		}
	}
	if _, err := s.CreateTag(t.Context(), owner, "No receipt", ""); !errors.Is(err, store.ErrIdempotencyKey) {
		t.Fatal("tag create did not require an explicit command key", err)
	}
	for _, limit := range []int{0, 102} {
		if _, err := s.Tags(t.Context(), owner, "", limit); !errors.Is(err, ErrGroupSet) {
			t.Fatal("unbounded catalog query was accepted", limit, err)
		}
	}
	if _, err := s.db.Exec(`UPDATE tags SET version=? WHERE id=?`, math.MaxInt64, tag.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameTag(t.Context(), owner, tag.ID, math.MaxInt64, "Overflow", store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("tag rename overflow was not rejected", err)
	}
	if err := s.DeleteTag(t.Context(), owner, tag.ID, math.MaxInt64, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("tag deletion overflow was not rejected", err)
	}
	history, err := s.CreateTag(t.Context(), owner, "Receipt target", store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	renameKey, deleteKey := store.NewID(), store.NewID()
	if _, err := s.RenameTag(t.Context(), owner, history.ID, 1, "Historical renamed", renameKey); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTag(t.Context(), owner, history.ID, 2, deleteKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET role='user',version=version+1 WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTag(t.Context(), owner, "Private unused label", key); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("historical create receipt bypassed fresh admin authorization", err)
	}
	if _, err := s.RenameTag(t.Context(), owner, history.ID, 1, "Historical renamed", renameKey); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("historical rename receipt bypassed fresh admin authorization", err)
	}
	if err := s.DeleteTag(t.Context(), owner, history.ID, 2, deleteKey); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("historical delete receipt bypassed fresh admin authorization", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Tags(ctx, owner, "", 30); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled catalog query proceeded", err)
	}
}

func TestTagCatalogStableIDKeysetsAndConcurrentCommands(t *testing.T) {
	s, owner := fixture(t)
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 105 {
		if _, err := tx.Exec(`INSERT INTO tags(id,name) VALUES(?,?)`, fmt.Sprintf("tag-%03d", index), fmt.Sprintf("Label %03d", index)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	first, err := s.Tags(t.Context(), owner, "", 100)
	if err != nil || len(first) != 100 {
		t.Fatal("catalog keyset did not bound its first page", first, err)
	}
	last, err := s.Tags(t.Context(), owner, first[99].ID, 100)
	if err != nil || len(last) != 5 || last[4].ID != "tag-104" {
		t.Fatal("catalog keyset lost or duplicated tags", last, err)
	}
	counts := tagCounts(t, s)
	key := store.NewID()
	type outcome struct {
		tag Tag
		err error
	}
	results := make(chan outcome, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			value, err := s.CreateTag(t.Context(), owner, "Concurrent", key)
			results <- outcome{value, err}
		})
	}
	group.Wait()
	close(results)
	var tag Tag
	for result := range results {
		if result.err != nil || result.tag.Version != 1 || result.tag.Name != "Concurrent" || (tag.ID != "" && tag.ID != result.tag.ID) {
			t.Fatal("same-key create raced past its receipt", result, tag)
		}
		tag = result.tag
	}
	after := tagCounts(t, s)
	for index := range counts {
		if after[index] != counts[index]+1 {
			t.Fatal("concurrent creation duplicated side effects", counts, after)
		}
	}
	conflicts := make(chan error, 2)
	for _, name := range []string{"First", "Second"} {
		group.Go(func() {
			_, err := s.RenameTag(t.Context(), owner, tag.ID, 1, name, store.NewID())
			conflicts <- err
		})
	}
	group.Wait()
	close(conflicts)
	succeeded, stale := 0, 0
	for err := range conflicts {
		if err == nil {
			succeeded++
		} else if errors.Is(err, auth.ErrConflict) {
			stale++
		} else {
			t.Fatal("unexpected concurrent rename result", err)
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatal("competing tag commands both applied one version", succeeded, stale)
	}
}

func TestTagCatalogLateJournalFailuresRollBackCreateRenameAndDelete(t *testing.T) {
	for _, operation := range []string{"create", "rename", "delete"} {
		t.Run(operation, func(t *testing.T) {
			s, owner := fixture(t)
			tag, err := s.CreateTag(t.Context(), owner, "Existing", store.NewID())
			if err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"change_log", "outbox_events", "audit_events", "mutation_receipts"} {
				t.Run(table, func(t *testing.T) {
					before, err := s.Tags(t.Context(), owner, "", 100)
					if err != nil {
						t.Fatal(err)
					}
					counts := tagCounts(t, s)
					if _, err := s.db.Exec(`CREATE TRIGGER reject_tag_journal BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'private tag journal failure'); END`); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if _, err := s.db.Exec(`DROP TRIGGER reject_tag_journal`); err != nil {
							t.Error(err)
						}
					})
					switch operation {
					case "create":
						_, err = s.CreateTag(t.Context(), owner, "New", store.NewID())
					case "rename":
						_, err = s.RenameTag(t.Context(), owner, tag.ID, 1, "New", store.NewID())
					case "delete":
						err = s.DeleteTag(t.Context(), owner, tag.ID, 1, store.NewID())
					}
					if err == nil {
						t.Fatal("late tag journal failure was ignored")
					}
					after, readErr := s.Tags(t.Context(), owner, "", 100)
					if readErr != nil || !reflect.DeepEqual(before, after) || tagCounts(t, s) != counts {
						t.Fatal("partial tag metadata/event/audit/receipt survived rollback", before, after, readErr)
					}
				})
			}
		})
	}
}
