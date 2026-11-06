package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeCoverBatches(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeTagBatchResults(t)
	data, err := fs.ReadFile(migrations.Files, "0018_album_tag_batch_results.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0018_album_tag_batch_results.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func TestCoverBatchMigrationPreservesTagHistoryAndConstrainsTypedSnapshots(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeCoverBatches(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at,scope_id) VALUES('actor','actor','not-a-login-hash','admin',0,0,'scope');
 INSERT INTO jobs(id,kind,payload_json,status,next_run_at,created_at,updated_at,created_by_user_id) VALUES('tag-job','batch_tags','{"version":1}','queued',0,0,0,'actor'),('cover-job','batch_covers','{"version":1}','queued',0,0,0,'actor');
 INSERT INTO album_tag_batches VALUES('tag-job','actor','[]',0);
 INSERT INTO album_tag_batch_targets VALUES('tag-job','tag-album',3,'library',NULL,NULL);
 UPDATE jobs SET status='running',attempts=1,lease_owner='private',lease_expires_at=100000 WHERE id='tag-job';
 INSERT INTO job_attempts(job_id,attempt,started_at,status) VALUES('tag-job',1,0,'running');
 INSERT INTO album_tag_batch_runs VALUES('tag-job',1,1,0);
 INSERT INTO album_tag_batch_results VALUES('tag-result','tag-job',1,'tag-album','unchanged',NULL,3,0)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var preserved, parents, targets, results int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_tag_batch_results WHERE id='tag-result' AND outcome='unchanged' AND album_version=3),(SELECT COUNT(*) FROM album_cover_batches),(SELECT COUNT(*) FROM album_cover_batch_targets),(SELECT COUNT(*) FROM album_cover_batch_results)`).Scan(&preserved, &parents, &targets, &results); err != nil || preserved != 1 || parents != 0 || targets != 0 || results != 0 {
		t.Fatal("migration altered tag history or invented cover work", preserved, parents, targets, results, err)
	}
	for _, query := range []string{
		`INSERT INTO album_cover_batches VALUES('tag-job','actor',0)`,
		`INSERT INTO album_cover_batches VALUES('cover-job','other-actor',0)`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("wrong cover job/actor accepted", query)
		}
	}
	if _, err := db.Exec(`INSERT INTO album_cover_batches VALUES('cover-job','actor',0);
 INSERT INTO album_cover_batch_targets VALUES('cover-job','a',7,'library','historical-source',4,'historical-media','historical-media',2,'historical-revision'),('cover-job','empty',1,'library',NULL,NULL,'',NULL,NULL,NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE album_cover_batches SET actor_user_id='another'`,
		`UPDATE album_cover_batch_targets SET requested_cover_id=''`,
		`INSERT INTO album_cover_batch_targets VALUES('cover-job','bad',1,'library','source',NULL,'',NULL,NULL,NULL)`,
		`INSERT INTO album_cover_batch_targets VALUES('cover-job','bad',1,'library',NULL,1,'',NULL,NULL,NULL)`,
		`INSERT INTO album_cover_batch_targets VALUES('cover-job','bad',1,'library',NULL,NULL,'selected',NULL,NULL,NULL)`,
		`INSERT INTO album_cover_batch_targets VALUES('cover-job','bad',1,'library',NULL,NULL,'selected','other',1,NULL)`,
		`INSERT INTO album_cover_batch_targets VALUES('cover-job','bad',1,'library',NULL,NULL,'selected','selected',NULL,NULL)`,
		`INSERT INTO album_cover_batch_targets VALUES('cover-job','bad',1,'library',NULL,NULL,'',NULL,1,NULL)`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("mutable/ambiguous cover snapshot accepted", query)
		}
	}
	if _, err := db.Exec(`UPDATE jobs SET status='running',attempts=1,lease_owner='private-cover',lease_expires_at=100000 WHERE id='cover-job';
 INSERT INTO job_attempts(job_id,attempt,started_at,status) VALUES('cover-job',1,0,'running');
 INSERT INTO album_cover_batch_runs VALUES('cover-job',1,2,0);
 INSERT INTO album_cover_batch_results VALUES('cover-result','cover-job',1,'a','applied',NULL,8,0)`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE album_cover_batch_runs SET target_count=1`,
		`UPDATE album_cover_batch_results SET album_version=9`,
		`INSERT INTO album_cover_batch_results VALUES('bad','cover-job',1,'empty','applied',NULL,NULL,0)`,
		`INSERT INTO album_cover_batch_targets VALUES('cover-job','late',1,'library',NULL,NULL,'',NULL,NULL,NULL)`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("late/mutable cover attempt accepted", query)
		}
	}
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT id FROM album_cover_batch_results WHERE job_id=? AND id>? ORDER BY id LIMIT 100`, "cover-job", "")
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
	if !strings.Contains(plan, "idx_album_cover_batch_result_page") {
		t.Fatal("cover result keysets lost indexed seek", plan)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id='actor'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM album_cover_batch_results WHERE id='cover-result' AND album_version=8`).Scan(&results); err != nil || results != 1 {
		t.Fatal("actor removal erased historical resolution", results, err)
	}
	if _, err := db.Exec(`DELETE FROM jobs WHERE id='cover-job'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM album_cover_batches),(SELECT COUNT(*) FROM album_cover_batch_targets),(SELECT COUNT(*) FROM album_cover_batch_results),(SELECT COUNT(*) FROM album_tag_batch_results)`).Scan(&parents, &targets, &results, &preserved); err != nil || parents != 0 || targets != 0 || results != 0 || preserved != 1 {
		t.Fatal("cover history cascade altered unrelated tag history", parents, targets, results, preserved, err)
	}
}

func TestCoverBatchMigrationLateDDLFailureRollsBackWithoutChangingOldTables(t *testing.T) {
	db := testDB(t)
	previous := beforeCoverBatches(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER album_cover_batch_result_immutable BEFORE UPDATE ON jobs BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("late cover DDL failure ignored")
	}
	var tables, ledger, indexes int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name LIKE 'album_cover_batch%'),(SELECT COUNT(*) FROM schema_migrations WHERE version='0019_album_cover_batches.sql'),(SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name LIKE 'idx_album_cover_batch%')`).Scan(&tables, &ledger, &indexes); err != nil || tables != 0 || ledger != 0 || indexes != 0 {
		t.Fatal("failed cover migration left partial schema", tables, ledger, indexes, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER album_cover_batch_result_immutable`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("cover migration could not retry cleanly", err)
	}
}
