package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"gallerygo/migrations"
)

func albumJournalBatchFixture(t *testing.T) *sql.DB {
	t.Helper()
	db := testDB(t)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	seedFavoriteFolderSelection(t, db)
	if _, err := db.Exec(`INSERT INTO tags(id,name) VALUES('tag-z','Z'),('tag-a','A');
 INSERT INTO album_tags(album_id,tag_id) VALUES('a','tag-z'),('a','tag-a');
 INSERT INTO album_views(user_id,album_id,view_count,last_viewed_at,version) VALUES('u','a',2,10,3);
 INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('cover-a','a','image','cover.png',0,0);
 UPDATE albums SET cover_media_id='cover-a' WHERE id='a';
 UPDATE albums SET quarantined_at=-1234,access_restricted=1 WHERE id='b'`); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAlbumJournalBatchMatchesCanonicalSingleWrites(t *testing.T) {
	db := albumJournalBatchFixture(t)
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	now := time.Date(2026, 11, 10, 12, 34, 56, 789000000, time.UTC)
	ids := []string{"a", "b", "c"}
	for _, id := range ids {
		if err := JournalAlbumTx(t.Context(), tx, id, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := JournalAlbumBatchTx(t.Context(), tx, ids, now); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(t.Context(), `SELECT c.resource_id,c.payload_json,c.version,c.actor_scope,c.operation,
 (SELECT COUNT(*) FROM outbox_events o WHERE o.change_seq=c.seq) FROM change_log c
 WHERE c.kind='album' AND c.created_at=? ORDER BY c.seq`, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	type event struct {
		id, payload, scope, operation string
		version                       int64
		outboxes                      int
	}
	var got []event
	for rows.Next() {
		var e event
		if err := rows.Scan(&e.id, &e.payload, &e.version, &e.scope, &e.operation, &e.outboxes); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		got = append(got, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if len(got) != 6 || !reflect.DeepEqual(got[:3], got[3:]) {
		t.Fatal("prepared batch did not preserve the exact canonical single-row events", got)
	}
	for _, e := range got {
		var fields map[string]json.RawMessage
		if json.Unmarshal([]byte(e.payload), &fields) != nil || len(fields) != 14 || e.scope != "system" || e.operation != "upsert" || e.outboxes != 1 {
			t.Fatal("prepared batch lost a complete event or durable outbox", e)
		}
	}
	var a struct {
		TagIDs       []string `json:"tag_ids"`
		ViewCount    int64    `json:"view_count"`
		CoverMediaID *string  `json:"cover_media_id"`
		RatingAvg    *float64 `json:"rating_avg"`
	}
	if err := json.Unmarshal([]byte(got[0].payload), &a); err != nil || !reflect.DeepEqual(a.TagIDs, []string{"tag-a", "tag-z"}) || a.ViewCount != 2 || a.CoverMediaID == nil || *a.CoverMediaID != "cover-a" || a.RatingAvg == nil || *a.RatingAvg != 5 {
		t.Fatal("canonical fixture lost tag order, actual views, cover or native mean", a, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAlbumJournalBatchRejectsInvalidPagesAndRollsBackLateFailures(t *testing.T) {
	db := albumJournalBatchFixture(t)
	stats := func() [2]int64 {
		t.Helper()
		var got [2]int64
		if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events)`).Scan(&got[0], &got[1]); err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := stats()
	now := time.Now().UTC()
	for _, ids := range [][]string{{"a", "b", "a"}, make([]string, 129), {"a", "b", "not-an-album"}} {
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := JournalAlbumBatchTx(t.Context(), tx, ids, now); err == nil {
			t.Fatal("duplicate, oversized or missing-ID journal page succeeded", len(ids), ids)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if got := stats(); got != before {
			t.Fatal("failed journal page leaked a change or outbox", before, got)
		}
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_batch_outbox BEFORE INSERT ON outbox_events
 WHEN EXISTS(SELECT 1 FROM change_log c WHERE c.seq=NEW.change_seq AND c.kind='album' AND c.resource_id='b' AND c.created_at>100)
 BEGIN SELECT RAISE(ABORT,'injected late batch outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalAlbumBatchTx(t.Context(), tx, []string{"a", "b", "c"}, now); err == nil || !strings.Contains(err.Error(), "injected late batch outbox failure") {
		t.Fatal("late batch outbox failure was not reported", err)
	}
	var partialChanges, partialOutboxes int
	if err := tx.QueryRow(`SELECT COUNT(*),COUNT(o.id) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq
 WHERE c.kind='album' AND c.created_at=?`, now.UnixMilli()).Scan(&partialChanges, &partialOutboxes); err != nil || partialChanges != 2 || partialOutboxes != 1 {
		t.Fatal("failure did not occur after a prior complete event and a later change", partialChanges, partialOutboxes, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := stats(); got != before {
		t.Fatal("late batch outbox failure leaked earlier changes", before, got)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_batch_outbox`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	tx, err = db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalAlbumBatchTx(ctx, tx, []string{"a"}, now); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled batch did not stop before publication", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := stats(); got != before {
		t.Fatal("cancelled batch leaked a change or outbox", before, got)
	}
}

func TestAlbumJournalBatchFullPageAndTagLimit(t *testing.T) {
	db := albumJournalBatchFixture(t)
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<128)
 INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at)
 SELECT printf('page-%03d',i),'l','s',printf('Name %03d',i),0,0 FROM n`); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT id FROM albums WHERE id LIKE 'page-%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if len(ids) != 128 {
		t.Fatal("full-page fixture size", len(ids))
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := JournalAlbumBatchTx(t.Context(), tx, nil, now); err != nil {
		t.Fatal("empty page", err)
	}
	if err := JournalAlbumBatchTx(t.Context(), tx, ids, now); err != nil {
		t.Fatal("full page", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var changes, outboxes int
	if err := db.QueryRow(`SELECT COUNT(*),COUNT(o.id) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq
 WHERE c.kind='album' AND c.resource_id LIKE 'page-%' AND c.created_at=?`, now.UnixMilli()).Scan(&changes, &outboxes); err != nil || changes != 128 || outboxes != 128 {
		t.Fatal("full page did not publish each canonical event and outbox", changes, outboxes, err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<99)
 INSERT INTO tags(id,name) SELECT printf('new-%03d',i),printf('New %03d',i) FROM n;
 INSERT INTO album_tags(album_id,tag_id) SELECT 'a',id FROM tags WHERE id LIKE 'new-%'`); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := JournalAlbumTx(t.Context(), tx, "a", now); !errors.Is(err, ErrAlbumTagSet) {
		t.Fatal("single projector did not reject 101 tags", err)
	}
	if err := JournalAlbumBatchTx(t.Context(), tx, []string{"b", "a"}, now); !errors.Is(err, ErrAlbumTagSet) {
		t.Fatal("batch projector changed the tag limit", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*),COUNT(o.id) FROM change_log c LEFT JOIN outbox_events o ON o.change_seq=c.seq
 WHERE c.kind='album' AND c.resource_id IN ('a','b') AND c.created_at=?`, now.UnixMilli()).Scan(&changes, &outboxes); err != nil || changes != 0 || outboxes != 0 {
		t.Fatal("tag-limit failure leaked earlier page events", changes, outboxes, err)
	}
}
