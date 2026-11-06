package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeTagBatches(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "0017_" {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: data}
	}
	return previous
}

func TestTagBatchMigrationPreservesExistingDataAndFreezesNewJobSnapshots(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeTagBatches(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at,scope_id) VALUES('actor','actor','not-a-login-hash','admin',0,0,'scope');
 INSERT INTO libraries(id,name,created_at) VALUES('library','Retained',0);
 INSERT INTO albums(id,library_id,source_name,display_name,version,access_restricted,created_at,updated_at) VALUES('album','library','Indexed','Override',9,1,0,0);
 INSERT INTO tags(id,name) VALUES('tag','Retained'); INSERT INTO album_tags(album_id,tag_id) VALUES('album','tag');
 INSERT INTO jobs(id,kind,payload_json,status,next_run_at,created_at,updated_at,created_by_user_id) VALUES
 ('batch-job','batch_tags','{}','queued',0,0,0,'actor'),('scan-job','scan','{}','queued',0,0,0,'actor'),('old-job','batch_tags','{}','cancelled',0,0,0,'actor')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var preserved bool
	var jobs, intents, targets int
	if err := db.QueryRow(`SELECT version=9 AND display_name='Override' AND access_restricted=1 AND (SELECT COUNT(*) FROM album_tags WHERE album_id='album')=1,
 (SELECT COUNT(*) FROM jobs),(SELECT COUNT(*) FROM album_tag_batches),(SELECT COUNT(*) FROM album_tag_batch_targets) FROM albums WHERE id='album'`).Scan(&preserved, &jobs, &intents, &targets); err != nil || !preserved || jobs != 3 || intents != 0 || targets != 0 {
		t.Fatal("migration changed metadata, jobs or invented accepted targets", preserved, jobs, intents, targets, err)
	}
	for _, query := range []string{
		`INSERT INTO album_tag_batches VALUES('scan-job','actor','[]',0)`,
		`INSERT INTO album_tag_batches VALUES('old-job','actor','[]',0)`,
		`INSERT INTO album_tag_batches VALUES('batch-job','another-actor','[]',0)`,
		`INSERT INTO album_tag_batches VALUES('missing-job','actor','[]',0)`,
		`INSERT INTO album_tag_batches VALUES('batch-job','actor','null',0)`,
		`INSERT INTO album_tag_batches VALUES('batch-job','actor','{}',0)`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("invalid batch parent/actor/body was accepted", query)
		}
	}
	if _, err := db.Exec(`INSERT INTO album_tag_batches VALUES('batch-job','actor','["tag"]',0);
 INSERT INTO album_tag_batch_targets VALUES('batch-job','album',9,'library','historical-source',4),('batch-job','local',1,'library',NULL,NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE album_tag_batches SET tag_ids_json='[]' WHERE job_id='batch-job'`,
		`UPDATE album_tag_batch_targets SET album_version=10 WHERE job_id='batch-job'`,
		`INSERT INTO album_tag_batch_targets VALUES('batch-job','invalid',0,'library',NULL,NULL)`,
		`INSERT INTO album_tag_batch_targets VALUES('batch-job','invalid',1,'library','source',NULL)`,
		`INSERT INTO album_tag_batch_targets VALUES('batch-job','invalid',1,'library','source',0)`,
		`INSERT INTO album_tag_batch_targets VALUES('batch-job','invalid',1,'library',NULL,1)`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("mutable or ambiguous target snapshot was accepted", query)
		}
	}
	if _, err := db.Exec(`UPDATE jobs SET version=2 WHERE id='batch-job'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO album_tag_batch_targets VALUES('batch-job','late',1,'library',NULL,NULL)`); err == nil {
		t.Fatal("existing job accepted expanded target snapshot")
	}
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT album_id FROM album_tag_batch_targets WHERE job_id=? AND album_id>? ORDER BY album_id LIMIT 100`, "batch-job", "")
	if err != nil {
		t.Fatal(err)
	}
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if !strings.Contains(plan, "PRIMARY KEY") {
		t.Fatal("batch keysets lost indexed seek", plan)
	}
	if _, err := db.Exec(`DELETE FROM albums; DELETE FROM users WHERE id='actor'`); err != nil {
		t.Fatal(err)
	}
	var actor, source string
	if err := db.QueryRow(`SELECT b.actor_user_id,t.source_id FROM album_tag_batches b JOIN album_tag_batch_targets t ON t.job_id=b.job_id WHERE t.album_id='album'`).Scan(&actor, &source); err != nil || actor != "actor" || source != "historical-source" {
		t.Fatal("resource/account removal erased historical IDs", actor, source, err)
	}
	if _, err := db.Exec(`DELETE FROM jobs WHERE id='batch-job'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_tag_batches),(SELECT COUNT(*) FROM album_tag_batch_targets)`).Scan(&intents, &targets); err != nil || intents != 0 || targets != 0 {
		t.Fatal("job-owned history failed to cascade", intents, targets, err)
	}
}

func TestTagBatchMigrationLateFailureRollsBackTablesTriggersAndLedger(t *testing.T) {
	db := testDB(t)
	previous := beforeTagBatches(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('l','Preserved',0);
 CREATE TRIGGER album_tag_batch_target_immutable BEFORE UPDATE ON libraries BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("injected late batch DDL collision was ignored")
	}
	var tables, triggers, ledger, libraries int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name IN ('album_tag_batches','album_tag_batch_targets')),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND name LIKE 'album_tag_batch%'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0017_album_tag_batches.sql'),(SELECT COUNT(*) FROM libraries WHERE id='l' AND name='Preserved')`).Scan(&tables, &triggers, &ledger, &libraries); err != nil || tables != 0 || triggers != 1 || ledger != 0 || libraries != 1 {
		t.Fatal("failed batch migration left partial schema/ledger/data", tables, triggers, ledger, libraries, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER album_tag_batch_target_immutable`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("batch migration could not retry cleanly", err)
	}
}
