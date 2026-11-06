package access

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func creatorFixture(t *testing.T) personFixture {
	t.Helper()
	f := indexedPeopleFixture(t)
	if _, err := f.svc.db.Exec(`UPDATE albums SET mapped_person_id=person_id`); err != nil {
		t.Fatal(err)
	}
	return f
}

func creatorCounts(t *testing.T, s *Service) [5]int64 {
	t.Helper()
	var value [5]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id='person-open'),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events WHERE event_type='album_creator_changed'),(SELECT COUNT(*) FROM mutation_receipts WHERE operation='album.creator.set'),(SELECT COUNT(*) FROM change_log WHERE kind='scope' AND operation='revoke')`).Scan(&value[0], &value[1], &value[2], &value[3], &value[4]); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestAlbumCreatorModesPreserveMappedFallbackAndHistoricalReceipts(t *testing.T) {
	f := creatorFixture(t)
	before, err := f.svc.AdminAlbumCreator(t.Context(), f.owner, "person-open")
	if err != nil || before.Mode != "mapped" || before.PersonID == nil || *before.PersonID != "person-one" || before.MappedPersonID == nil || *before.MappedPersonID != "person-one" || before.OverridePersonID != nil || before.Version != 1 {
		t.Fatal("incorrect initial creator provenance", before, err)
	}
	counts := creatorCounts(t, f.svc)
	key := store.NewID()
	input := AlbumCreatorRequest{Mode: "person", PersonID: new("person-two")}
	changed, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, input, key)
	if err != nil || changed.Mode != "person" || changed.PersonID == nil || *changed.PersonID != "person-two" || changed.OverridePersonID == nil || *changed.OverridePersonID != "person-two" || changed.MappedPersonID == nil || *changed.MappedPersonID != "person-one" || changed.Version != 2 {
		t.Fatal("manual creator choice lost indexed fallback or identity", changed, err)
	}
	after := creatorCounts(t, f.svc)
	if after[0] != counts[0]+1 || after[2] != counts[2]+1 || after[3] != counts[3]+1 || after[4] <= counts[4] || after[1]-counts[1] != 1+after[4]-counts[4] {
		t.Fatal("creator graph/scopes/change/outbox/audit/receipt did not co-commit", counts, after)
	}
	assertAlbumEventTags(t, f.svc, "person-open", []string{})
	if _, err := f.svc.Person(t.Context(), f.base, "person-one"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("old creator retained a hidden-only album label for this user", err)
	}
	person, err := f.svc.Person(t.Context(), f.base, "person-two")
	if err != nil || person.AlbumCount != 2 {
		t.Fatal("new creator aggregate lost the explicitly classified visible album", person, err)
	}
	replay, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, input, key)
	if err != nil || !reflect.DeepEqual(replay, changed) || creatorCounts(t, f.svc) != after {
		t.Fatal("creator retry duplicated events or lost historical result", replay, changed, err)
	}
	noop, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 2, input, store.NewID())
	if err != nil || !reflect.DeepEqual(noop, changed) {
		t.Fatal("same creator preference was not a no-op", noop, err)
	}
	noopCounts := creatorCounts(t, f.svc)
	if noopCounts[0] != after[0] || noopCounts[1] != after[1] || noopCounts[2] != after[2] || noopCounts[3] != after[3]+1 || noopCounts[4] != after[4] {
		t.Fatal("creator no-op changed resource/events/scopes", after, noopCounts)
	}
	none, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 2, AlbumCreatorRequest{Mode: "none"}, store.NewID())
	if err != nil || none.Mode != "none" || none.PersonID != nil || none.OverridePersonID != nil || none.MappedPersonID == nil || *none.MappedPersonID != "person-one" || none.Version != 3 {
		t.Fatal("explicit no-creator mode discarded indexed fallback", none, err)
	}
	// Simulate an already committed newer indexed mapping, without rewriting the
	// manual choice. Restoring mapped mode must use this value, never an old ID.
	if _, err := f.svc.db.Exec(`UPDATE albums SET mapped_person_id='person-two' WHERE id='person-open'`); err != nil {
		t.Fatal(err)
	}
	restored, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 3, AlbumCreatorRequest{Mode: "mapped"}, store.NewID())
	if err != nil || restored.Mode != "mapped" || restored.PersonID == nil || *restored.PersonID != "person-two" || restored.OverridePersonID != nil || restored.Version != 4 {
		t.Fatal("mapped restoration used stale or guessed provenance", restored, err)
	}
	replay, err = f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, input, key)
	if err != nil || !reflect.DeepEqual(replay, changed) {
		t.Fatal("later classification retargeted an old creator receipt", replay, changed, err)
	}
	if _, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, input, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("new stale creator command ignored shared album version", err)
	}
	if _, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, AlbumCreatorRequest{Mode: "none"}, key); !errors.Is(err, store.ErrIdempotencyReuse) {
		t.Fatal("creator key accepted another classification", err)
	}
}

func TestAlbumCreatorValidationSourceIdentityRolesAndPreferenceOnlyScopes(t *testing.T) {
	f := creatorFixture(t)
	before := creatorCounts(t, f.svc)
	for _, input := range []AlbumCreatorRequest{{}, {Mode: "unknown"}, {Mode: "person"}, {Mode: "mapped", PersonID: new("")}, {Mode: "none", PersonID: new("person-two")}} {
		if _, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, input, store.NewID()); !errors.Is(err, ErrCreatorMode) {
			t.Fatal("ambiguous creator mode was accepted", input, err)
		}
	}
	for _, id := range []string{"", "missing", "person-hidden", "person-no-path", "person-root-path", "person-unclassified", strings.Repeat("x", 65), "bad\x00id"} {
		if _, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, AlbumCreatorRequest{Mode: "person", PersonID: &id}, store.NewID()); !errors.Is(err, ErrCreatorSelection) {
			t.Fatal("foreign/unbound/invalid creator was selected", id, err)
		}
	}
	if creatorCounts(t, f.svc) != before {
		t.Fatal("invalid classification changed state or events")
	}
	forged := f.base
	forged.User.Role = "admin"
	for _, viewer := range []auth.Session{f.base, forged} {
		if _, err := f.svc.AdminAlbumCreator(t.Context(), viewer, "person-open"); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role exposed creator preferences", err)
		}
		if _, err := f.svc.SetAlbumCreator(t.Context(), viewer, "person-open", 1, AlbumCreatorRequest{Mode: "none"}, store.NewID()); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller role edited creator classification", err)
		}
	}
	// Latching the current mapped creator changes preference but not the visible
	// graph, so it must not manufacture an authorization-scope rotation.
	key := store.NewID()
	input := AlbumCreatorRequest{Mode: "person", PersonID: new("person-one")}
	latched, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, input, key)
	if err != nil || latched.Version != 2 {
		t.Fatal(err, latched)
	}
	after := creatorCounts(t, f.svc)
	if after[4] != before[4] {
		t.Fatal("preference-only latch revoked unchanged creator graph", before, after)
	}
	if _, err := f.svc.db.Exec(`UPDATE users SET role='user',version=version+1 WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, input, key); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("historical creator receipt bypassed fresh admin authorization", err)
	}
	if _, err := f.svc.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.db.Exec(`UPDATE albums SET version=? WHERE id='person-open'`, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", math.MaxInt64, AlbumCreatorRequest{Mode: "none"}, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("creator resource version overflow was accepted", err)
	}
	if _, err := f.svc.db.Exec(`UPDATE sources SET is_active=0 WHERE id='person-source'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AdminAlbumCreator(t.Context(), f.owner, "person-open"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("inactive creator source exposed preferences", err)
	}
	if _, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", math.MaxInt64, AlbumCreatorRequest{Mode: "none"}, store.NewID()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("inactive creator source accepted classification", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.svc.SetAlbumCreator(ctx, f.owner, "person-open", 2, AlbumCreatorRequest{Mode: "none"}, store.NewID()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled classification proceeded", err)
	}
}

func TestAlbumCreatorConcurrentCommandsAndLateFailuresAreAtomic(t *testing.T) {
	f := creatorFixture(t)
	key := store.NewID()
	input := AlbumCreatorRequest{Mode: "person", PersonID: new("person-two")}
	type outcome struct {
		value AlbumCreator
		err   error
	}
	results := make(chan outcome, 8)
	var group sync.WaitGroup
	counts := creatorCounts(t, f.svc)
	for range 8 {
		group.Go(func() {
			value, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 1, input, key)
			results <- outcome{value, err}
		})
	}
	group.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.value.Version != 2 || result.value.PersonID == nil || *result.value.PersonID != "person-two" {
			t.Fatal("same-key creator command raced past its receipt", result)
		}
	}
	after := creatorCounts(t, f.svc)
	if after[0] != counts[0]+1 || after[2] != counts[2]+1 || after[3] != counts[3]+1 || after[1]-counts[1] != 1+after[4]-counts[4] {
		t.Fatal("concurrent creator receipt duplicated business/scope events", counts, after)
	}
	for _, table := range []string{"change_log", "outbox_events", "audit_events", "mutation_receipts"} {
		t.Run(table, func(t *testing.T) {
			before, err := f.svc.AdminAlbumCreator(t.Context(), f.owner, "person-open")
			if err != nil {
				t.Fatal(err)
			}
			counts := creatorCounts(t, f.svc)
			var oldScope string
			if err := f.svc.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.base.User.ID).Scan(&oldScope); err != nil {
				t.Fatal(err)
			}
			condition := ""
			if table == "change_log" {
				condition = " WHEN NEW.kind='album'"
			}
			if _, err := f.svc.db.Exec(`CREATE TRIGGER reject_creator_journal BEFORE INSERT ON ` + table + condition + ` BEGIN SELECT RAISE(ABORT,'private creator journal failure'); END`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if _, err := f.svc.db.Exec(`DROP TRIGGER reject_creator_journal`); err != nil {
					t.Error(err)
				}
			})
			if _, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 2, AlbumCreatorRequest{Mode: "none"}, store.NewID()); err == nil {
				t.Fatal("late creator journal failure was ignored")
			}
			current, err := f.svc.AdminAlbumCreator(t.Context(), f.owner, "person-open")
			var scope string
			scopeErr := f.svc.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.base.User.ID).Scan(&scope)
			if err != nil || scopeErr != nil || scope != oldScope || !reflect.DeepEqual(before, current) || creatorCounts(t, f.svc) != counts {
				t.Fatal("creator graph/version/scopes/events survived rollback", before, current, oldScope, scope, err, scopeErr)
			}
		})
	}
	conflicts := make(chan error, 2)
	for _, mode := range []string{"mapped", "none"} {
		group.Go(func() {
			_, err := f.svc.SetAlbumCreator(t.Context(), f.owner, "person-open", 2, AlbumCreatorRequest{Mode: mode}, store.NewID())
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
			t.Fatal(err)
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatal("competing creator choices both applied one album version", succeeded, stale)
	}
}
