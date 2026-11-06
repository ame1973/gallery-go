package access

import (
	"context"
	"database/sql"
	"encoding/json"
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

type personFixture struct {
	svc               *Service
	owner, base, both auth.Session
	privateGroup      Group
}

func indexedPeopleFixture(t *testing.T) personFixture {
	t.Helper()
	s, owner := fixture(t)
	baseGroup, privateGroup := group(t, s, owner, "Person library"), group(t, s, owner, "Person private")
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('person-library','One',0),('other-person-library','Other',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('person-source','person-library','one','/private/never-open-person-source','{}','unavailable',0),('other-person-source','other-person-library','other','/private/never-open-other-person-source','{}','healthy',0);
 INSERT INTO people(id,source_id,relative_path,source_name,display_name,created_at) VALUES
 ('person-one','person-source','private/first/Creator','Same label','Visible alias',0),
 ('person-two','person-source','private/second/Creator','Same label',NULL,0),
 ('person-hidden','other-person-source','private/foreign/Creator','Hidden creator',NULL,0),
 ('person-empty','person-source','private/empty','Empty',NULL,0),
 ('person-no-path','person-source',NULL,'Not indexed',NULL,0),
 ('person-root-path','person-source','.','Not mapped',NULL,0),
 ('person-unclassified',NULL,NULL,'Unclassified',NULL,0);
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,cover_media_id,created_at,updated_at) VALUES
 ('person-open','person-library','person-source','person-one','Open','a-secret-media',0,0),
 ('person-private','person-library','person-source','person-one','Private','a-secret-media',0,0),
 ('person-other','person-library','person-source','person-two','Other',NULL,0,0),
 ('person-foreign','other-person-library','other-person-source','person-hidden','Foreign',NULL,0,0),
 ('person-cross-link','person-library','person-source','person-hidden','Cross link',NULL,0,0),
 ('person-no-path-album','person-library','person-source','person-no-path','Not indexed',NULL,0,0),
 ('person-root-path-album','person-library','person-source','person-root-path','Not mapped',NULL,0,0),
 ('person-unclassified-album','person-library','person-source','person-unclassified','Unclassified',NULL,0,0);
 INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES
 ('z-visible-media','person-open','image','visible.jpg',0,0),
 ('a-secret-media','person-private','image','secret.jpg',0,0),
 ('archived-cover','person-open','image','archived.jpg',0,0);
 UPDATE media SET availability='archived' WHERE id='archived-cover';
 INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,updated_at) VALUES('wrong-person-entry','other-person-source','never-read.jpg',1,0,0);
 INSERT INTO media(id,album_id,source_entry_id,kind,file_name,created_at,updated_at) VALUES('0-wrong-source-media','person-open','wrong-person-entry','image','wrong.jpg',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLibraryPolicy(t.Context(), owner, "person-library", 1, []string{baseGroup.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAlbumPolicy(t.Context(), owner, "person-private", 1, true, []string{privateGroup.ID}); err != nil {
		t.Fatal(err)
	}
	base, both := reader(t, s, "person-base"), reader(t, s, "person-both")
	membership(t, s, owner, base, baseGroup.ID)
	membership(t, s, owner, both, baseGroup.ID, privateGroup.ID)
	return personFixture{s, owner, base, both, privateGroup}
}

func personMetadataCounts(t *testing.T, s *Service) [4]int64 {
	t.Helper()
	var counts [4]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log WHERE kind='person' AND resource_id='person-one'),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events WHERE event_type='person_metadata_updated'),(SELECT COUNT(*) FROM mutation_receipts WHERE operation='person.metadata.update')`).Scan(&counts[0], &counts[1], &counts[2], &counts[3]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestPersonMetadataOverrideClearReplayAndCanonicalEvents(t *testing.T) {
	f := indexedPeopleFixture(t)
	before, err := f.svc.AdminPersonMetadata(t.Context(), f.owner, "person-one")
	if err != nil || before.SourceID != "person-source" || before.SourceName != "Same label" || before.DisplayName == nil || *before.DisplayName != "Visible alias" || before.Version != 1 {
		t.Fatal("incorrect administrative person label projection", before, err)
	}
	counts := personMetadataCounts(t, f.svc)
	key := store.NewID()
	patch := PersonMetadataPatch{DisplayName: new("  Edited creator  ")}
	changed, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 1, patch, key)
	if err != nil || changed.Version != 2 || changed.DisplayName == nil || *changed.DisplayName != "Edited creator" || changed.SourceID != before.SourceID || changed.SourceName != before.SourceName {
		t.Fatal("person display override did not preserve indexed identity/label", changed, err)
	}
	afterCounts := personMetadataCounts(t, f.svc)
	for index := range counts {
		if afterCounts[index] != counts[index]+1 {
			t.Fatal("person row/event/outbox/audit/receipt did not co-commit", counts, afterCounts)
		}
	}
	public, err := f.svc.Person(t.Context(), f.base, before.ID)
	if err != nil || public.Name != "Edited creator" || public.Version != 2 || public.AlbumCount != 1 {
		t.Fatal("authorized public person did not project the display override", public, err)
	}
	replay, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 1, PersonMetadataPatch{DisplayName: new("Edited creator")}, key)
	if err != nil || !reflect.DeepEqual(replay, changed) || personMetadataCounts(t, f.svc) != afterCounts {
		t.Fatal("canonical actor receipt did not return exactly one historical mutation", replay, changed, err)
	}
	noop, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 2, patch, store.NewID())
	if err != nil || !reflect.DeepEqual(noop, changed) {
		t.Fatal("same display preference was not a version/event no-op", noop, err)
	}
	noopCounts := personMetadataCounts(t, f.svc)
	if noopCounts[0] != afterCounts[0] || noopCounts[1] != afterCounts[1] || noopCounts[2] != afterCounts[2] || noopCounts[3] != afterCounts[3]+1 {
		t.Fatal("no-op wrote business events or lost its receipt", afterCounts, noopCounts)
	}
	cleared, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 2, PersonMetadataPatch{DisplayName: new("")}, store.NewID())
	if err != nil || cleared.Version != 3 || cleared.DisplayName != nil {
		t.Fatal("explicit empty label did not clear only the override", cleared, err)
	}
	public, err = f.svc.Person(t.Context(), f.base, before.ID)
	if err != nil || public.Name != before.SourceName || public.Version != 3 {
		t.Fatal("clear did not restore the indexed source label", public, err)
	}
	replay, err = f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 1, patch, key)
	if err != nil || !reflect.DeepEqual(replay, changed) {
		t.Fatal("receipt retry after a later edit lost its historical result", replay, err)
	}
	if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 1, patch, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("new stale command ignored the current person version", err)
	}
	for _, input := range []struct {
		id      string
		version int64
		name    string
	}{{before.ID, 1, "Different"}, {before.ID, 2, "Edited creator"}, {"person-two", 1, "Edited creator"}} {
		if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, input.id, input.version, PersonMetadataPatch{DisplayName: &input.name}, key); !errors.Is(err, store.ErrIdempotencyReuse) {
			t.Fatal("receipt key was retargeted to another body/version/person", input, err)
		}
	}
	var source, path, label, association string
	var restricted bool
	if err := f.svc.db.QueryRow(`SELECT p.source_id,p.relative_path,p.source_name,a.person_id,a.access_restricted FROM people p JOIN albums a ON a.id='person-private' WHERE p.id='person-one'`).Scan(&source, &path, &label, &association, &restricted); err != nil || source != "person-source" || path != "private/first/Creator" || label != before.SourceName || association != before.ID || !restricted {
		t.Fatal("person label mutation rewrote identity/association/restriction", source, path, label, association, restricted, err)
	}
	var payload string
	if err := f.svc.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='person' AND resource_id='person-one' ORDER BY seq LIMIT 1`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var event map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &event) != nil || len(event) != 5 {
		t.Fatal("person event is not the complete private-safe canonical projection", payload)
	}
	for _, field := range []string{"id", "source_id", "source_name", "display_name", "version"} {
		if _, ok := event[field]; !ok {
			t.Fatal("person mutation omitted a canonical event field", field, payload)
		}
	}
}

func TestPersonMetadataValidationVisibilityAndFreshAuthorization(t *testing.T) {
	f := indexedPeopleFixture(t)
	before, _ := f.svc.AdminPersonMetadata(t.Context(), f.owner, "person-one")
	counts := personMetadataCounts(t, f.svc)
	if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 1, PersonMetadataPatch{}, store.NewID()); !errors.Is(err, ErrPersonMetadata) {
		t.Fatal("empty person metadata patch was accepted", err)
	}
	for _, name := range []string{" ", "bad\x00name", "bad\xff", strings.Repeat("字", 101)} {
		if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 1, PersonMetadataPatch{DisplayName: &name}, store.NewID()); !errors.Is(err, ErrName) {
			t.Fatal("invalid display label was accepted or implicitly cleared", name, err)
		}
	}
	after, err := f.svc.AdminPersonMetadata(t.Context(), f.owner, before.ID)
	if err != nil || !reflect.DeepEqual(before, after) || personMetadataCounts(t, f.svc) != counts {
		t.Fatal("invalid commands changed state or business events", before, after, err)
	}
	for _, id := range []string{"person-empty", "person-no-path", "person-root-path", "person-unclassified", "missing"} {
		if _, err := f.svc.AdminPersonMetadata(t.Context(), f.owner, id); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("unmapped or empty person metadata exposed its label", id, err)
		}
		if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, id, 1, PersonMetadataPatch{DisplayName: new("Denied")}, store.NewID()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("unmapped or empty person became administratively editable", id, err)
		}
	}
	forged := f.base
	forged.User.Role = "admin"
	for _, session := range []auth.Session{f.base, forged} {
		if _, err := f.svc.AdminPersonMetadata(t.Context(), session, before.ID); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller DTO role authorized person metadata", err)
		}
		if _, err := f.svc.UpdatePersonMetadata(t.Context(), session, before.ID, 1, PersonMetadataPatch{DisplayName: new("Denied")}, store.NewID()); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller DTO role authorized person mutation", err)
		}
	}
	key := store.NewID()
	patch := PersonMetadataPatch{DisplayName: new("Receipt")}
	if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 1, patch, key); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.db.Exec(`UPDATE users SET role='user',version=version+1 WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 1, patch, key); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("historical receipt bypassed current administrator authorization", err)
	}
	if _, err := f.svc.db.Exec(`UPDATE users SET role='admin' WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.db.Exec(`UPDATE people SET version=? WHERE id='person-two'`, math.MaxInt64); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, "person-two", math.MaxInt64, patch, store.NewID()); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("person metadata version overflow was not rejected", err)
	}
	for _, statement := range []string{
		`UPDATE sources SET is_active=0 WHERE id='person-source'`,
		`UPDATE sources SET is_active=1 WHERE id='person-source'; UPDATE libraries SET is_active=0 WHERE id='person-library'`,
		`UPDATE libraries SET is_active=1 WHERE id='person-library'; UPDATE sources SET library_id='other-person-library' WHERE id='person-source'`,
	} {
		if _, err := f.svc.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.AdminPersonMetadata(t.Context(), f.owner, before.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("inactive or inconsistent source bypassed admin metadata visibility", statement, err)
		}
		if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 2, patch, store.NewID()); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("inactive or inconsistent source was edited", statement, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.svc.UpdatePersonMetadata(ctx, f.owner, before.ID, 2, patch, store.NewID()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled person mutation proceeded", err)
	}
}

func TestPersonMetadataConcurrentCommandsCommitOneBusinessWrite(t *testing.T) {
	f := indexedPeopleFixture(t)
	counts := personMetadataCounts(t, f.svc)
	key := store.NewID()
	patch := PersonMetadataPatch{DisplayName: new("Concurrent creator")}
	type outcome struct {
		value PersonMetadata
		err   error
	}
	results := make(chan outcome, 8)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			value, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, "person-one", 1, patch, key)
			results <- outcome{value, err}
		})
	}
	group.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.value.Version != 2 || result.value.DisplayName == nil || *result.value.DisplayName != "Concurrent creator" {
			t.Fatal("same-key concurrent command lost its historical result", result.value, result.err)
		}
	}
	after := personMetadataCounts(t, f.svc)
	for index := range counts {
		if after[index] != counts[index]+1 {
			t.Fatal("concurrent person commands duplicated business side effects", counts, after)
		}
	}
	conflicts := make(chan error, 2)
	for _, title := range []string{"First", "Second"} {
		group.Go(func() {
			_, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, "person-one", 2, PersonMetadataPatch{DisplayName: &title}, store.NewID())
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
			t.Fatal("unexpected concurrent version result", err)
		}
	}
	if succeeded != 1 || stale != 1 {
		t.Fatal("distinct commands both used one person version", succeeded, stale)
	}
}

func TestPersonMetadataLateJournalFailureRollsBackEverything(t *testing.T) {
	for _, table := range []string{"change_log", "outbox_events", "audit_events", "mutation_receipts"} {
		t.Run(table, func(t *testing.T) {
			f := indexedPeopleFixture(t)
			before, _ := f.svc.AdminPersonMetadata(t.Context(), f.owner, "person-one")
			counts := personMetadataCounts(t, f.svc)
			if _, err := f.svc.db.Exec(`CREATE TRIGGER reject_person_metadata BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected private journal failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.svc.UpdatePersonMetadata(t.Context(), f.owner, before.ID, 1, PersonMetadataPatch{DisplayName: new("Must roll back")}, store.NewID()); err == nil {
				t.Fatal("late journal/receipt failure was ignored")
			}
			after, err := f.svc.AdminPersonMetadata(t.Context(), f.owner, before.ID)
			if err != nil || !reflect.DeepEqual(before, after) || personMetadataCounts(t, f.svc) != counts {
				t.Fatal("partial person metadata/events survived a failed transaction", before, after, err)
			}
		})
	}
}

func TestPeopleListsDetailsCountsCoversAndAlbumsShareFreshSQLVisibility(t *testing.T) {
	f := indexedPeopleFixture(t)
	for _, test := range []struct {
		viewer auth.Session
		count  int64
		cover  string
		total  int
	}{{f.base, 1, "z-visible-media", 2}, {f.both, 2, "a-secret-media", 2}, {f.owner, 2, "a-secret-media", 3}} {
		people, err := f.svc.People(t.Context(), test.viewer, "", 30)
		if err != nil || len(people) != test.total {
			t.Fatal("person name visibility differs from album access", test, people, err)
		}
		person, err := f.svc.Person(t.Context(), test.viewer, "person-one")
		if err != nil || person.ID != "person-one" || person.Name != "Visible alias" || person.SourceID != "person-source" || person.AlbumCount != test.count || person.CoverMediaID == nil || *person.CoverMediaID != test.cover || person.Version != 1 {
			t.Fatal("person aggregates/cover leaked a hidden or foreign resource", test, person, err)
		}
		albums, err := f.svc.PersonAlbums(t.Context(), test.viewer, "person-one", "", 30)
		if err != nil || int64(len(albums)) != test.count {
			t.Fatal("person album list differs from its visible count", albums, person, err)
		}
		for _, listed := range people {
			detail, err := f.svc.Person(t.Context(), test.viewer, listed.ID)
			if err != nil || detail.ID != listed.ID || detail.Name != listed.Name || detail.AlbumCount != listed.AlbumCount {
				t.Fatal("person list/detail predicate mismatch", listed, detail, err)
			}
		}
		encoded, _ := json.Marshal(people)
		for _, private := range []string{"relative_path", "root_path", "/private", "private/first", "private/second", "marker"} {
			if strings.Contains(string(encoded), private) {
				t.Fatal("person DTO exposed private source identity/path", private, string(encoded))
			}
		}
	}
	for _, id := range []string{"person-empty", "person-no-path", "person-root-path", "person-unclassified", "missing"} {
		for _, viewer := range []auth.Session{f.owner, f.base} {
			if _, err := f.svc.Person(t.Context(), viewer, id); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("unbound/empty/unknown person became public", id, err)
			}
			if _, err := f.svc.PersonAlbums(t.Context(), viewer, id, "", 30); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("hidden person's albums revealed existence", id, err)
			}
		}
	}
	if _, err := f.svc.Person(t.Context(), f.base, "person-hidden"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross-source album association revealed another creator", err)
	}
	second, err := f.svc.Person(t.Context(), f.base, "person-two")
	if err != nil || second.Name != "Same label" || second.CoverMediaID != nil || second.AlbumCount != 1 {
		t.Fatal("empty visible album has no valid nullable cover", second, err)
	}
}

func TestPeopleKeepSourcePathIdentitiesAndRecheckRevokedMemberships(t *testing.T) {
	f := indexedPeopleFixture(t)
	if err := f.svc.DeleteGroup(t.Context(), f.owner, f.privateGroup.ID, f.privateGroup.Version); err != nil {
		t.Fatal(err)
	}
	person, err := f.svc.Person(t.Context(), f.both, "person-one")
	if err != nil || person.AlbumCount != 1 || person.CoverMediaID == nil || *person.CoverMediaID != "z-visible-media" {
		t.Fatal("deleted private group broadened creator aggregates", person, err)
	}
	var restricted bool
	if err := f.svc.db.QueryRow(`SELECT access_restricted FROM albums WHERE id='person-private'`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("creator read cleared persistent album restriction", restricted, err)
	}
	if _, err := f.svc.db.Exec(`UPDATE people SET display_name=NULL WHERE id='person-one'`); err != nil {
		t.Fatal(err)
	}
	people, err := f.svc.People(t.Context(), f.base, "", 30)
	if err != nil || len(people) != 2 || people[0].Name != "Same label" || people[1].Name != "Same label" || people[0].ID == people[1].ID {
		t.Fatal("equal source labels collapsed separate full-path identities", people, err)
	}
	for _, statement := range []string{
		`UPDATE sources SET is_active=0 WHERE id='person-source'`,
		`UPDATE sources SET is_active=1; UPDATE libraries SET is_active=0 WHERE id='person-library'`,
		`UPDATE libraries SET is_active=1; UPDATE sources SET library_id='other-person-library' WHERE id='person-source'`,
	} {
		if _, err := f.svc.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		for _, viewer := range []auth.Session{f.base, f.owner} {
			if _, err := f.svc.Person(t.Context(), viewer, "person-one"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("inactive/reassigned source bypassed person visibility", statement, err)
			}
		}
	}
}

func TestPeopleAndPersonAlbumsUseBoundedStableIDKeysets(t *testing.T) {
	s, owner := fixture(t)
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('page-library','Pages',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,created_at) VALUES('page-source','page-library','pages','/never-open-pages','{}',0)`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 105 {
		id := fmt.Sprintf("person-%03d", index)
		if _, err := tx.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES(?,'page-source',?,'Same',0)`, id, "private/"+id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES(?,'page-library','page-source',?,'Album',0,0)`, "album-"+id, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	first, err := s.People(t.Context(), owner, "", 100)
	if err != nil || len(first) != 100 {
		t.Fatal("person keyset is not bounded", first, err)
	}
	last, err := s.People(t.Context(), owner, first[99].ID, 100)
	if err != nil || len(last) != 5 || last[0].ID != "person-100" || last[4].ID != "person-104" {
		t.Fatal("person keyset lost/duplicated IDs", last, err)
	}
	if _, err := s.db.Exec(`UPDATE albums SET person_id='person-000'`); err != nil {
		t.Fatal(err)
	}
	firstAlbums, err := s.PersonAlbums(t.Context(), owner, "person-000", "", 100)
	if err != nil || len(firstAlbums) != 100 {
		t.Fatal("person albums page is not bounded", firstAlbums, err)
	}
	lastAlbums, err := s.PersonAlbums(t.Context(), owner, "person-000", firstAlbums[99].ID, 100)
	if err != nil || len(lastAlbums) != 5 {
		t.Fatal("person album cursor lost rows", lastAlbums, err)
	}
	for _, limit := range []int{0, 102} {
		if _, err := s.People(t.Context(), owner, "", limit); !errors.Is(err, ErrGroupSet) {
			t.Fatal("invalid person page limit accepted", err)
		}
		if _, err := s.PersonAlbums(t.Context(), owner, "person-000", "", limit); !errors.Is(err, ErrGroupSet) {
			t.Fatal("invalid person album limit accepted", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.People(ctx, owner, "", 30); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled person query was not stopped", err)
	}
}
