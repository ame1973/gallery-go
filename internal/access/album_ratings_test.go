package access

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/store"
)

func ratingFixture(t *testing.T) (*Service, auth.Session, auth.Session) {
	t.Helper()
	s, owner := albumMetadataFixture(t)
	g := group(t, s, owner, "Voters")
	viewer := reader(t, s, "voter")
	membership(t, s, owner, viewer, g.ID)
	if _, err := s.SetLibraryPolicy(t.Context(), owner, "metadata-library", 1, []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAlbumPolicy(t.Context(), owner, "metadata-album", 1, true, []string{g.ID}); err != nil {
		t.Fatal(err)
	}
	var err error
	owner, err = s.auth.Authenticate(t.Context(), owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	viewer, err = s.auth.Authenticate(t.Context(), viewer.Token)
	if err != nil {
		t.Fatal(err)
	}
	return s, owner, viewer
}

func ratingCounts(t *testing.T, s *Service) [5]int64 {
	t.Helper()
	var counts [5]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM ratings),(SELECT COUNT(*) FROM change_log),
 (SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events WHERE event_type='album_rating_changed'),
 (SELECT COUNT(*) FROM mutation_receipts WHERE operation='album.rating.replace')`).Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func assertRatingAverage(t *testing.T, s *Service, viewer auth.Session, id string, want *float64) Album {
	t.Helper()
	value, err := s.Album(t.Context(), viewer, id)
	if err != nil || !reflect.DeepEqual(value.RatingAvg, want) {
		t.Fatal("shared SQL average differs from live votes", value, want, err)
	}
	return value
}

func TestAlbumRatingsOwnVersionsReplacementClearAndHistoricalReceipts(t *testing.T) {
	s, owner, viewer := ratingFixture(t)
	const id = "metadata-album"
	instant := time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)
	s.now = func() time.Time { return instant }
	initial, err := s.AlbumRating(t.Context(), owner, id)
	if err != nil || initial.AlbumID != id || initial.Rating != nil || initial.Version != 1 || initial.UpdatedAt != nil {
		t.Fatal("never-rated state was invented or unversioned", initial, err)
	}
	album := assertRatingAverage(t, s, viewer, id, nil)
	key := store.NewID()
	five := 5
	changed, err := s.SetAlbumRating(t.Context(), owner, id, 1, &five, key)
	if err != nil || changed.Version != 2 || changed.Rating == nil || *changed.Rating != 5 || changed.UpdatedAt == nil || !changed.UpdatedAt.Equal(instant) {
		t.Fatal("first vote did not publish its own revision/server time", changed, err)
	}
	if value := assertRatingAverage(t, s, viewer, id, new(5.0)); value.Version != album.Version+1 {
		t.Fatal("rating changed the average without the shared album version", value)
	}
	assertAlbumEventTags(t, s, id, []string{})
	counts := ratingCounts(t, s)
	noop, err := s.SetAlbumRating(t.Context(), owner, id, 2, &five, store.NewID())
	afterNoop := ratingCounts(t, s)
	if err != nil || !reflect.DeepEqual(noop, changed) || afterNoop[4] != counts[4]+1 {
		t.Fatal("same vote did not save an exact no-op receipt", noop, counts, afterNoop, err)
	}
	afterNoop[4]--
	if afterNoop != counts {
		t.Fatal("no-op rewrote business rows, events or audit", counts, afterNoop)
	}
	other, err := s.AlbumRating(t.Context(), viewer, id)
	if err != nil || other.Version != 1 || other.Rating != nil {
		t.Fatal("one account's vote changed another private resource", other, err)
	}
	three := 3
	other, err = s.SetAlbumRating(t.Context(), viewer, id, 1, &three, store.NewID())
	if err != nil || other.Version != 2 {
		t.Fatal("second authorized voter could not use their independent revision", other, err)
	}
	assertRatingAverage(t, s, viewer, id, new(4.0))
	four := 4
	if _, err := s.SetAlbumRating(t.Context(), viewer, id, 2, &four, store.NewID()); err != nil {
		t.Fatal(err)
	}
	assertRatingAverage(t, s, owner, id, new(4.5))
	own, err := s.AlbumRating(t.Context(), owner, id)
	if err != nil || !reflect.DeepEqual(own, changed) {
		t.Fatal("another voter invalidated the personal revision", own, changed, err)
	}
	cleared, err := s.SetAlbumRating(t.Context(), owner, id, 2, nil, store.NewID())
	if err != nil || cleared.Version != 3 || cleared.Rating != nil {
		t.Fatal("clear did not leave a versioned null state", cleared, err)
	}
	assertRatingAverage(t, s, viewer, id, new(4.0))
	var tombstone int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ratings WHERE user_id=? AND album_id=? AND rating IS NULL AND version=3`, owner.User.ID, id).Scan(&tombstone); err != nil || tombstone != 1 {
		t.Fatal("clear physically erased the personal revision", tombstone, err)
	}
	var scope, operation, payload string
	if err := s.db.QueryRow(`SELECT actor_scope,operation,payload_json FROM change_log WHERE kind='album_rating' AND resource_id=? ORDER BY seq DESC LIMIT 1`, id).Scan(&scope, &operation, &payload); err != nil {
		t.Fatal(err)
	}
	var event AlbumRating
	if json.Unmarshal([]byte(payload), &event) != nil || scope != owner.User.ScopeID || operation != "delete" || !reflect.DeepEqual(event, cleared) {
		t.Fatal("private rating tombstone lost its actor scope/current state", scope, operation, payload)
	}
	replay, err := s.SetAlbumRating(t.Context(), owner, id, 1, &five, key)
	if err != nil || !reflect.DeepEqual(replay, changed) {
		t.Fatal("historical receipt was recomputed after replacement/clear", replay, changed, err)
	}
	current, _ := s.AlbumRating(t.Context(), owner, id)
	if !reflect.DeepEqual(current, cleared) {
		t.Fatal("receipt replay restored an obsolete vote", current, cleared)
	}
	for _, version := range []int64{1, 2} {
		_, err := s.SetAlbumRating(t.Context(), owner, id, version, &five, store.NewID())
		requireError(t, err, auth.ErrConflict)
	}
	_, err = s.SetAlbumRating(t.Context(), owner, id, 1, &four, key)
	requireError(t, err, store.ErrIdempotencyReuse)
	_, err = s.SetAlbumRating(t.Context(), owner, id, 2, &five, key)
	requireError(t, err, store.ErrIdempotencyReuse)
	counts = ratingCounts(t, s)
	noop, err = s.SetAlbumRating(t.Context(), owner, id, 3, nil, store.NewID())
	if err != nil || !reflect.DeepEqual(noop, cleared) {
		t.Fatal("repeated clear changed its tombstone", noop, err)
	}
	afterNoop = ratingCounts(t, s)
	afterNoop[4]--
	if afterNoop != counts {
		t.Fatal("repeated clear wrote business effects", counts, afterNoop)
	}
	s.now = func() time.Time { return instant.Add(-time.Minute) }
	one := 1
	recreated, err := s.SetAlbumRating(t.Context(), owner, id, 3, &one, store.NewID())
	if err != nil || recreated.Version != 4 || recreated.UpdatedAt == nil || !recreated.UpdatedAt.Equal(instant) {
		t.Fatal("recreated vote reused a revision or moved its server clock backwards", recreated, err)
	}
	assertRatingAverage(t, s, viewer, id, new(2.5))
	s.now = func() time.Time { return instant.Add(25 * time.Hour) }
	_, err = s.SetAlbumRating(t.Context(), owner, id, 1, &five, key)
	requireError(t, err, auth.ErrConflict)
	if _, err := s.SetAlbumRating(t.Context(), owner, id, 4, &one, key); err != nil {
		t.Fatal("expired receipt key could not be reused with the current revision", err)
	}
	var missing, votes int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE o.id IS NULL),
 (SELECT COUNT(*) FROM ratings WHERE album_id=? AND rating IS NOT NULL)`, id).Scan(&missing, &votes); err != nil || missing != 0 || votes != 2 {
		t.Fatal("votes duplicated or change/outbox diverged", missing, votes, err)
	}
}

func TestAlbumRatingsFreshVisibilityAndActualActorPrecedeReceiptReplay(t *testing.T) {
	s, owner, viewer := ratingFixture(t)
	const id = "metadata-album"
	key, five := store.NewID(), 5
	forged := viewer
	forged.User = owner.User
	if _, err := s.SetAlbumRating(t.Context(), forged, id, 1, &five, key); err != nil {
		t.Fatal(err)
	}
	own, _ := s.AlbumRating(t.Context(), owner, id)
	if own.Rating != nil || own.Version != 1 {
		t.Fatal("forged user DTO targeted another account", own)
	}
	for _, invalid := range []int{-1, 0, 6, math.MaxInt} {
		_, err := s.SetAlbumRating(t.Context(), viewer, id, 2, &invalid, store.NewID())
		requireError(t, err, ErrAlbumRating)
	}
	_, err := s.SetAlbumRating(t.Context(), viewer, id, 2, nil, "invalid")
	requireError(t, err, store.ErrIdempotencyKey)
	for _, query := range []string{
		`UPDATE libraries SET is_active=0 WHERE id='metadata-library'`,
		`UPDATE sources SET is_active=0 WHERE id='metadata-source'`,
		`UPDATE sources SET library_id='metadata-destination' WHERE id='metadata-source'`,
	} {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO libraries(id,name,created_at) VALUES('metadata-destination','Other',0)`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(query); err != nil {
			t.Fatal(err)
		}
		counts := ratingCounts(t, s)
		for _, actor := range []auth.Session{viewer, owner} {
			_, err := s.AlbumRating(t.Context(), actor, id)
			requireError(t, err, sql.ErrNoRows)
			_, err = s.SetAlbumRating(t.Context(), actor, id, 1, &five, key)
			requireError(t, err, sql.ErrNoRows)
		}
		if ratingCounts(t, s) != counts {
			t.Fatal("hidden resource request returned/changed an old receipt")
		}
		if _, err := s.db.Exec(`UPDATE libraries SET is_active=1 WHERE id='metadata-library'; UPDATE sources SET is_active=1,library_id='metadata-library' WHERE id='metadata-source'`); err != nil {
			t.Fatal(err)
		}
	}
	membership(t, s, owner, viewer)
	_, err = s.SetAlbumRating(t.Context(), viewer, id, 1, &five, key)
	requireError(t, err, sql.ErrNoRows)
	_, err = s.AlbumRating(t.Context(), viewer, id)
	requireError(t, err, sql.ErrNoRows)
	_, err = s.AlbumRating(t.Context(), owner, "missing")
	requireError(t, err, sql.ErrNoRows)
	if _, err := s.db.Exec(`UPDATE users SET active=0 WHERE id=?`, viewer.User.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.SetAlbumRating(t.Context(), viewer, id, 1, &five, key)
	requireError(t, err, auth.ErrUnauthenticated)
	_, err = s.AlbumRating(t.Context(), auth.Session{}, id)
	requireError(t, err, auth.ErrUnauthenticated)
}

func TestAlbumRatingsEveryLateFailureRollsBackVotesVersionsEventsAndReceipts(t *testing.T) {
	s, owner := albumMetadataFixture(t)
	for _, trigger := range []string{
		`CREATE TRIGGER fail_rating BEFORE INSERT ON change_log WHEN NEW.kind='album' BEGIN SELECT RAISE(ABORT,'injected shared journal failure'); END`,
		`CREATE TRIGGER fail_rating BEFORE INSERT ON change_log WHEN NEW.kind='album_rating' BEGIN SELECT RAISE(ABORT,'injected private journal failure'); END`,
		`CREATE TRIGGER fail_rating BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log c WHERE c.seq=NEW.change_seq AND c.kind='album_rating') BEGIN SELECT RAISE(ABORT,'injected private outbox failure'); END`,
		`CREATE TRIGGER fail_rating BEFORE INSERT ON audit_events WHEN NEW.event_type='album_rating_changed' BEGIN SELECT RAISE(ABORT,'injected audit failure'); END`,
		`CREATE TRIGGER fail_rating BEFORE INSERT ON mutation_receipts WHEN NEW.operation='album.rating.replace' BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`,
	} {
		before := ratingCounts(t, s)
		if _, err := s.db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetAlbumRating(t.Context(), owner, "metadata-album", 1, new(5), store.NewID()); err == nil {
			t.Fatal("rating committed without every late effect", trigger)
		}
		if ratingCounts(t, s) != before {
			t.Fatal("failed rating retained partial effects", before, ratingCounts(t, s))
		}
		if value := assertRatingAverage(t, s, owner, "metadata-album", nil); value.Version != 1 {
			t.Fatal("failed rating advanced album version", value)
		}
		if _, err := s.db.Exec(`DROP TRIGGER fail_rating`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAlbumRatingsConcurrentStaleWritersReplayAndIndependentVoters(t *testing.T) {
	s, owner, viewer := ratingFixture(t)
	for _, scenario := range []struct {
		id     string
		replay bool
	}{{"metadata-album", false}, {"metadata-empty", true}} {
		before := ratingCounts(t, s)
		key := store.NewID()
		start := make(chan struct{})
		results := make(chan error, 8)
		var workers sync.WaitGroup
		for range 8 {
			workers.Go(func() {
				<-start
				requestKey := key
				if !scenario.replay {
					requestKey = store.NewID()
				}
				value, err := s.SetAlbumRating(t.Context(), owner, scenario.id, 1, new(5), requestKey)
				if err == nil && (value.Version != 2 || value.Rating == nil || *value.Rating != 5) {
					err = errors.New("concurrent success returned an incorrect personal result")
				}
				results <- err
			})
		}
		close(start)
		workers.Wait()
		close(results)
		applied, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				applied++
			case errors.Is(err, auth.ErrConflict):
				conflicts++
			default:
				t.Fatal("concurrent writer failed outside the version contract", err)
			}
		}
		if scenario.replay && (applied != 8 || conflicts != 0) || !scenario.replay && (applied != 1 || conflicts != 7) {
			t.Fatal("concurrent stale/replay requests applied incorrectly", scenario, applied, conflicts)
		}
		after := ratingCounts(t, s)
		want := before
		for index, delta := range [5]int64{1, 2, 2, 1, 1} {
			want[index] += delta
		}
		if after != want {
			t.Fatal("concurrent requests duplicated business effects", before, after, want)
		}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for index, actor := range []auth.Session{owner, viewer} {
		go func() {
			<-start
			_, err := s.SetAlbumRating(t.Context(), actor, "metadata-other", 1, new(1+index*4), store.NewID())
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal("independent personal revisions conflicted", err)
		}
	}
	assertRatingAverage(t, s, viewer, "metadata-other", new(3.0))
}

func TestAlbumRatingVersionOverflowCannotWrapOrPartiallyApply(t *testing.T) {
	s, owner := albumMetadataFixture(t)
	if _, err := s.db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at,version) VALUES(?,'metadata-album',5,0,?)`, owner.User.ID, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	before := ratingCounts(t, s)
	_, err := s.SetAlbumRating(t.Context(), owner, "metadata-album", math.MaxInt64, new(4), store.NewID())
	requireError(t, err, auth.ErrConflict)
	if ratingCounts(t, s) != before {
		t.Fatal("personal overflow partially applied")
	}
	if _, err := s.db.Exec(`UPDATE albums SET version=? WHERE id='metadata-empty'`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	_, err = s.SetAlbumRating(t.Context(), owner, "metadata-empty", 1, new(5), store.NewID())
	requireError(t, err, auth.ErrConflict)
	if ratingCounts(t, s) != before {
		t.Fatal("shared overflow partially applied")
	}
	value, err := s.SetAlbumRating(t.Context(), owner, "metadata-album", math.MaxInt64, new(5), store.NewID())
	if err != nil || value.Version != math.MaxInt64 {
		t.Fatal("overflow guard rejected a real no-op", value, err)
	}
}
