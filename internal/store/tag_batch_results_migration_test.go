package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeTagBatchResults(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeTagBatches(t)
	data, err := fs.ReadFile(migrations.Files, "0017_album_tag_batches.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0017_album_tag_batches.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func TestTagBatchResultMigrationRetainsIntentAndFencesImmutableAttemptHistory(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeTagBatchResults(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at,scope_id) VALUES('actor','actor','not-a-login-hash','admin',0,0,'scope');
 INSERT INTO jobs(id,kind,payload_json,status,next_run_at,created_at,updated_at,created_by_user_id) VALUES('job','batch_tags','{"version":1}','queued',0,0,0,'actor');
 INSERT INTO album_tag_batches VALUES('job','actor','["historical-tag"]',0);
 INSERT INTO album_tag_batch_targets VALUES('job','a',7,'library','source',4),('job','b',3,'library','source',4),('job','local',2,'library',NULL,NULL),('job','invalid',2,'library',NULL,NULL);
 UPDATE jobs SET status='running',attempts=1,lease_owner='private-owner',lease_expires_at=100000 WHERE id='job';
 INSERT INTO job_attempts(job_id,attempt,started_at,status) VALUES('job',1,1,'running')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var intents, targets, runs, results int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_tag_batches WHERE actor_user_id='actor' AND tag_ids_json='["historical-tag"]'),(SELECT COUNT(*) FROM album_tag_batch_targets),(SELECT COUNT(*) FROM album_tag_batch_runs),(SELECT COUNT(*) FROM album_tag_batch_results)`).Scan(&intents, &targets, &runs, &results); err != nil || intents != 1 || targets != 4 || runs != 0 || results != 0 {
		t.Fatal("result migration changed intent or invented work", intents, targets, runs, results, err)
	}
	for _, query := range []string{
		`INSERT INTO album_tag_batch_runs VALUES('job',0,3,1)`,
		`INSERT INTO album_tag_batch_runs VALUES('job',2,4,1)`,
		`INSERT INTO album_tag_batch_runs VALUES('job',1,0,1)`,
		`INSERT INTO album_tag_batch_runs VALUES('job',1,101,1)`,
		`INSERT INTO album_tag_batch_runs VALUES('job',1,4,100001)`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("invalid/live-attempt work budget was accepted", query)
		}
	}
	if _, err := db.Exec(`INSERT INTO album_tag_batch_runs VALUES('job',1,4,1);
 INSERT INTO album_tag_batch_results VALUES('r-a','job',1,'a','applied',NULL,8,2),('r-b','job',1,'b','failed','TARGET_CHANGED',NULL,3),('r-local','job',1,'local','unchanged',NULL,2,4)`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE album_tag_batch_runs SET target_count=2`,
		`UPDATE album_tag_batch_results SET outcome='failed',error_code='TARGET_CHANGED',album_version=NULL WHERE id='r-a'`,
		`INSERT INTO album_tag_batch_results VALUES('bad','job',1,'missing','applied',NULL,1,1)`,
		`INSERT INTO album_tag_batch_results VALUES('bad','job',1,'a','applied',NULL,8,2)`,
		`INSERT INTO album_tag_batch_results VALUES('bad','job',1,'invalid','unknown',NULL,8,2)`,
		`INSERT INTO album_tag_batch_results VALUES('bad','job',1,'invalid','applied',NULL,NULL,2)`,
		`INSERT INTO album_tag_batch_results VALUES('bad','job',1,'invalid','failed',NULL,NULL,2)`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("mutable/ambiguous/duplicate target result was accepted", query)
		}
	}
	if _, err := db.Exec(`UPDATE jobs SET attempts=2 WHERE id='job'; INSERT INTO job_attempts(job_id,attempt,started_at,status) VALUES('job',2,10,'running'); INSERT INTO album_tag_batch_runs VALUES('job',2,4,10)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO album_tag_batch_results VALUES('duplicate-resolution','job',2,'a','applied',NULL,9,11)`); err == nil {
		t.Fatal("resolved target could apply a second time")
	}
	if _, err := db.Exec(`INSERT INTO album_tag_batch_results VALUES('retry-skip','job',2,'a','already_applied',NULL,8,11)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO album_tag_batch_results VALUES('stale','job',1,'b','failed','TARGET_CHANGED',NULL,12)`); err == nil {
		t.Fatal("old attempt wrote new result")
	}
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT id FROM album_tag_batch_results WHERE job_id=? AND id>? ORDER BY id LIMIT 100`, "job", "")
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
	if !strings.Contains(plan, "idx_album_tag_batch_result_page") {
		t.Fatal("result keysets lost indexed seek", plan)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id='actor'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_tag_batch_results`).Scan(&results); err != nil || results != 4 {
		t.Fatal("actor removal erased historical outcomes", results, err)
	}
	if _, err := db.Exec(`DELETE FROM jobs WHERE id='job'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_tag_batch_runs),(SELECT COUNT(*) FROM album_tag_batch_results)`).Scan(&runs, &results); err != nil || runs != 0 || results != 0 {
		t.Fatal("job-owned result history did not cascade", runs, results, err)
	}
}

func TestTagBatchResultMigrationLateFailureRollsBackAllNewProjectionTables(t *testing.T) {
	db := testDB(t)
	previous := beforeTagBatchResults(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER album_tag_batch_result_immutable BEFORE UPDATE ON jobs BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("late result DDL failure was ignored")
	}
	var tables, ledger, indexes int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM sqlite_schema WHERE name IN ('album_tag_batch_runs','album_tag_batch_results')),(SELECT COUNT(*) FROM schema_migrations WHERE version='0018_album_tag_batch_results.sql'),(SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name LIKE 'idx_album_tag_batch_%')`).Scan(&tables, &ledger, &indexes); err != nil || tables != 0 || ledger != 0 || indexes != 0 {
		t.Fatal("failed result migration left partial tables/indexes/ledger", tables, ledger, indexes, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER album_tag_batch_result_immutable`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("result migration did not retry cleanly", err)
	}
}
