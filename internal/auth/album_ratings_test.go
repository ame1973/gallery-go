package auth

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func ratingDeleteFixture(t *testing.T) (*Service, Session, User) {
	t.Helper()
	s, _ := testService(t)
	owner := login(t, s)
	user := managedUser(t, s, owner, "voter", "user")
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,is_active,created_at) VALUES('l','Retained',0,0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,is_active,created_at) VALUES('s','l','Source','/never-open-deleted-voter','{}','unavailable',0,0);
 INSERT INTO albums(id,library_id,source_id,source_name,display_name,version,created_at,updated_at) VALUES
 ('a','l','s','First','Override',7,-1234,99),('b','l','s','Second',NULL,8,0,0),('c','l','s','Cleared',NULL,9,0,0),('d','l','s','Other voter',NULL,10,0,0);
 INSERT INTO tags(id,name) VALUES('t','Retained tag'); INSERT INTO album_tags VALUES('a','t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at,version) VALUES
 (?,'a',5,-1234,7),(?,'a',1,4321,2),(?,'b',3,4567,4),(?,'c',NULL,9999,9),(?,'d',2,0,1)`,
		user.ID, owner.User.ID, user.ID, user.ID, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	return s, owner, user
}

func ratingDeleteCounts(t *testing.T, s *Service) [6]int64 {
	t.Helper()
	var value [6]int64
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM users),(SELECT COUNT(*) FROM ratings),
 (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events),
 (SELECT SUM(version) FROM albums)`).Scan(&value[0], &value[1], &value[2], &value[3], &value[4], &value[5]); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestDeleteUserRatingsRefreshEveryAffectedCanonicalAlbumIncludingHiddenSources(t *testing.T) {
	s, owner, user := ratingDeleteFixture(t)
	before := ratingDeleteCounts(t, s)
	if err := s.DeleteUser(t.Context(), owner, user.ID, user.Version); err != nil {
		t.Fatal(err)
	}
	want := before
	for i, delta := range [6]int64{-1, -3, 4, 4, 1, 2} {
		want[i] += delta
	}
	if actual := ratingDeleteCounts(t, s); actual != want {
		t.Fatal("account/votes/aggregate versions/revoke/tombstone/audit did not co-commit", actual, want)
	}
	for _, item := range []struct {
		id      string
		version int64
		average *float64
	}{{"a", 8, new(1.0)}, {"b", 9, nil}, {"c", 9, nil}, {"d", 10, new(2.0)}} {
		var version int64
		var average *float64
		if err := s.db.QueryRow(`SELECT a.version,(SELECT AVG(rating) FROM ratings WHERE album_id=a.id) FROM albums a WHERE id=?`, item.id).Scan(&version, &average); err != nil || version != item.version || !reflect.DeepEqual(average, item.average) {
			t.Fatal("delete changed the wrong album or retained an obsolete average", item, version, average, err)
		}
		if item.id == "c" || item.id == "d" {
			continue
		}
		var data string
		if err := s.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='album' AND resource_id=?`, item.id).Scan(&data); err != nil {
			t.Fatal(err)
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &payload) != nil || len(payload) != 13 {
			t.Fatal("deleted voter emitted a partial or different album projection", data)
		}
		var projected *float64
		if json.Unmarshal(payload["rating_avg"], &projected) != nil || !reflect.DeepEqual(projected, item.average) {
			t.Fatal("album journal did not refresh the removed vote", data, item.average)
		}
	}
	var retained, missing, changedEmpty int
	if err := s.db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM albums WHERE id='a' AND source_id='s' AND source_name='First' AND display_name='Override' AND created_at=-1234),
 (SELECT COUNT(*) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq WHERE o.id IS NULL),
 (SELECT COUNT(*) FROM change_log WHERE kind='album' AND resource_id IN('c','d'))`).Scan(&retained, &missing, &changedEmpty); err != nil || retained != 1 || missing != 0 || changedEmpty != 0 {
		t.Fatal("account deletion rewrote shared identity or journaled a non-vote", retained, missing, changedEmpty, err)
	}
}

func TestDeleteUserRatingsLateFailuresRollBackPreviouslyProcessedAlbumsAndCascade(t *testing.T) {
	s, owner, user := ratingDeleteFixture(t)
	before := ratingDeleteCounts(t, s)
	for _, trigger := range []string{
		`CREATE TRIGGER fail_delete_rating BEFORE INSERT ON change_log WHEN NEW.kind='album' AND NEW.resource_id='b' BEGIN SELECT RAISE(ABORT,'injected second album journal failure'); END`,
		`CREATE TRIGGER fail_delete_rating BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log c WHERE c.seq=NEW.change_seq AND c.kind='album' AND c.resource_id='b') BEGIN SELECT RAISE(ABORT,'injected second album outbox failure'); END`,
		`CREATE TRIGGER fail_delete_rating BEFORE DELETE ON users WHEN OLD.username='voter' BEGIN SELECT RAISE(ABORT,'injected user cascade failure'); END`,
		`CREATE TRIGGER fail_delete_rating BEFORE INSERT ON change_log WHEN NEW.kind='user' AND NEW.operation='delete' BEGIN SELECT RAISE(ABORT,'injected user tombstone failure'); END`,
		`CREATE TRIGGER fail_delete_rating BEFORE INSERT ON audit_events WHEN NEW.event_type='user_deleted' BEGIN SELECT RAISE(ABORT,'injected final audit failure'); END`,
	} {
		if _, err := s.db.Exec(trigger); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteUser(t.Context(), owner, user.ID, user.Version); err == nil {
			t.Fatal("delete committed without every affected album/account effect", trigger)
		}
		if actual := ratingDeleteCounts(t, s); actual != before {
			t.Fatal("late deletion failure retained partial votes/versions/events/account", actual, before)
		}
		if _, err := s.db.Exec(`DROP TRIGGER fail_delete_rating`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE albums SET version=? WHERE id='b'`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	// Album a sorts first and was processed before this genuine later overflow.
	assertError(t, s.DeleteUser(t.Context(), owner, user.ID, user.Version), ErrConflict)
	var first, votes, users int
	if err := s.db.QueryRow(`SELECT (SELECT version FROM albums WHERE id='a'),
 (SELECT COUNT(*) FROM ratings WHERE user_id=?),(SELECT COUNT(*) FROM users WHERE id=?)`, user.ID, user.ID).Scan(&first, &votes, &users); err != nil || first != 7 || votes != 3 || users != 1 {
		t.Fatal("overflow deletion left an earlier album or user partially removed", first, votes, users, err)
	}
}

func TestDeleteUserWithOnlyRatingTombstonesDoesNotInventAlbumChanges(t *testing.T) {
	s, owner, user := ratingDeleteFixture(t)
	if _, err := s.db.Exec(`UPDATE ratings SET rating=NULL WHERE user_id=?`, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(t.Context(), owner, user.ID, user.Version); err != nil {
		t.Fatal(err)
	}
	var changes, retained int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log WHERE kind='album'),
 (SELECT COUNT(*) FROM albums WHERE (id='a' AND version=7) OR (id='b' AND version=8) OR (id='c' AND version=9) OR (id='d' AND version=10))`).Scan(&changes, &retained); err != nil || changes != 0 || retained != 4 {
		t.Fatal("null tombstone cascade invented a shared average mutation", changes, retained, err)
	}
	var dangling int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&dangling); err != nil || dangling != 0 {
		t.Fatal("deleting rating tombstones violated foreign keys", dangling, err)
	}
}
