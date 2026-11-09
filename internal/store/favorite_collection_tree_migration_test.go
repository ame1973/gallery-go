package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeFavoriteCollectionTree(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := beforeFavoriteSelection(t)
	data, err := fs.ReadFile(migrations.Files, "0027_favorite_selection.sql")
	if err != nil {
		t.Fatal(err)
	}
	previous["0027_favorite_selection.sql"] = &fstest.MapFile{Data: data}
	return previous
}

func seedFavoriteCollectionTree(t *testing.T, db *sql.DB) {
	t.Helper()
	seedFavoriteSelectionUpgrade(t, db)
	if _, err := db.Exec(`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES
 ('a-root','u',NULL,'Root',-1),('b-child','u','a-root','Child',10),
 ('c-grand','u','b-child','Grand',20),('d-sibling','u','a-root','Sibling',30),
 ('v-root','v',NULL,'Root',40);
 UPDATE favorites SET collection_id='b-child' WHERE user_id='u' AND album_id='a'`); err != nil {
		t.Fatal(err)
	}
}

func TestFavoriteCollectionTreeMigrationPreservesOwnedRowsFavoritesAndCascades(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeFavoriteCollectionTree(t)); err != nil {
		t.Fatal(err)
	}
	seedFavoriteCollectionTree(t, db)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var folders, favorites, album, history, outbox, index, triggers, ledger, violations, fk int
	if err := db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM favorite_collections WHERE version=1 AND updated_at=created_at AND
 ((id='a-root' AND user_id='u' AND parent_id IS NULL AND created_at=-1) OR
 (id='b-child' AND user_id='u' AND parent_id='a-root' AND created_at=10) OR
 (id='c-grand' AND user_id='u' AND parent_id='b-child' AND created_at=20) OR
 (id='d-sibling' AND user_id='u' AND parent_id='a-root' AND created_at=30) OR
 (id='v-root' AND user_id='v' AND parent_id IS NULL AND created_at=40) OR
 (id='selection-folder' AND user_id='u' AND parent_id IS NULL AND created_at=0))),
 (SELECT COUNT(*) FROM favorites WHERE (user_id='u' AND album_id='a' AND collection_id='b-child' AND favorited=1 AND updated_at=456 AND version=3) OR (user_id='u' AND album_id='b' AND collection_id IS NULL AND favorited=0 AND version=4) OR (user_id='v' AND album_id='a' AND favorited=1 AND version=2)),
 (SELECT COUNT(*) FROM albums WHERE id='a' AND display_name='Override' AND version=7 AND updated_at=99),
 (SELECT COUNT(*) FROM change_log WHERE resource_id='a' AND payload_json='{"historical":"unchanged"}'),
 (SELECT COUNT(*) FROM outbox_events WHERE id='rating-history'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='idx_collection_owner_parent_page'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND name IN ('favorite_collection_insert_guard','favorite_collection_identity_guard','favorite_collection_parent_guard','favorite_collection_name_guard')),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0028_favorite_collection_tree.sql'),
 (SELECT COUNT(*) FROM pragma_foreign_key_check),(SELECT foreign_keys FROM pragma_foreign_keys)`).Scan(&folders, &favorites, &album, &history, &outbox, &index, &triggers, &ledger, &violations, &fk); err != nil || folders != 6 || favorites != 3 || album != 1 || history != 1 || outbox != 1 || index != 1 || triggers != 4 || ledger != 1 || violations != 0 || fk != 1 {
		t.Fatal("tree migration changed owned data/history/FKs or missed integrity schema", folders, favorites, album, history, outbox, index, triggers, ledger, violations, fk, err)
	}

	for _, query := range []string{
		`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('x','u','v-root','X',0)`,
		`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('x','u','missing','X',0)`,
		`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('x','u','x','X',0)`,
		`INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES(NULL,'u','X',0)`,
		`INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES('','u','X',0)`,
		`INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES(printf('%065d',1),'u','X',0)`,
		`INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES(CAST(X'610062' AS TEXT),'u','X',0)`,
		`UPDATE favorite_collections SET parent_id='c-grand' WHERE id='a-root'`,
		`UPDATE favorite_collections SET parent_id='b-child' WHERE id='b-child'`,
		`UPDATE favorite_collections SET parent_id='v-root' WHERE id='b-child'`,
		`UPDATE favorite_collections SET parent_id='missing' WHERE id='b-child'`,
		`UPDATE favorite_collections SET user_id='v' WHERE id='a-root'`,
		`UPDATE favorite_collections SET id='new-root' WHERE id='a-root'`,
		`INSERT OR REPLACE INTO favorite_collections(id,user_id,name,created_at) VALUES('v-root','u','Other',0)`,
		`INSERT OR REPLACE INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('new-child','u','a-root','Child',0)`,
		`UPDATE OR REPLACE favorite_collections SET name='Child' WHERE id='d-sibling'`,
		`DELETE FROM favorite_collections WHERE id='a-root'`,
		`DELETE FROM favorite_collections WHERE id='b-child'`,
		`UPDATE favorite_collections SET version=0 WHERE id='a-root'`,
		`UPDATE favorite_collections SET version=1.5 WHERE id='a-root'`,
		`UPDATE favorite_collections SET updated_at='bad' WHERE id='a-root'`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("collection hierarchy, ownership, identity, history or collision guard was bypassed", query)
		}
	}
	for _, query := range []string{
		`UPDATE favorite_collections SET parent_id=NULL WHERE id='b-child'`,
		`UPDATE favorite_collections SET parent_id='a-root' WHERE id='b-child'`,
		`UPDATE favorite_collections SET parent_id='a-root' WHERE id='c-grand'`,
		`UPDATE favorite_collections SET parent_id='b-child' WHERE id='c-grand'`,
		`UPDATE favorite_collections SET id=id,user_id=user_id,parent_id=parent_id WHERE id='a-root'`,
		`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('same-name-other-parent','u','b-child','Sibling',0)`,
		`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('same-name-other-owner','v','v-root','Child',0)`,
		`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at) VALUES('same-parent-case-distinct','u','a-root','sibling',0)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal("valid own-tree mutation failed", query, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES(?,'u','64-byte ID',0)`, strings.Repeat("字", 21)+"a"); err != nil {
		t.Fatal("valid 64-byte UTF-8 identity was rejected", err)
	}
	if _, err := db.Exec(`UPDATE favorite_collections SET version=2,updated_at=50 WHERE id='b-child'`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("applied tree migration was not idempotent", err)
	}
	var intact int
	if err := db.QueryRow(`SELECT COUNT(*) FROM favorite_collections WHERE (id='b-child' AND user_id='u' AND parent_id='a-root' AND name='Child' AND version=2 AND updated_at=50) OR (id='d-sibling' AND user_id='u' AND parent_id='a-root' AND name='Sibling') OR (id='v-root' AND user_id='v')`).Scan(&intact); err != nil || intact != 3 {
		t.Fatal("rejected REPLACE or hierarchy change erased a collection", intact, err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id='u'`); err != nil {
		t.Fatal("same-statement owner cascade could not remove nested collections and favorites", err)
	}
	var own, other, remainingAlbums int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE user_id='u'),(SELECT COUNT(*) FROM favorites WHERE user_id='v' AND album_id='a' AND favorited=1),(SELECT COUNT(*) FROM albums)`).Scan(&own, &other, &remainingAlbums); err != nil || own != 0 || other != 1 || remainingAlbums != 2 {
		t.Fatal("owner cascade retained private folders or altered another owner/shared albums", own, other, remainingAlbums, err)
	}
}

func TestFavoriteCollectionTreeConcurrentReparentCannotCreateCycle(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,scope_id,created_at,updated_at) VALUES('u','tree-race','not-a-login-hash','user','race-scope',0,0);
 INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES('left','u','Left',0),('right','u','Right',0)`); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, move := range []struct{ id, parent string }{{"left", "right"}, {"right", "left"}} {
		workers.Go(func() {
			<-start
			_, err := db.ExecContext(t.Context(), `UPDATE favorite_collections SET parent_id=? WHERE id=?`, move.parent, move.id)
			results <- err
		})
	}
	close(start)
	workers.Wait()
	close(results)
	success, rejected := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if strings.Contains(err.Error(), "collection hierarchy cannot contain a cycle") {
			rejected++
		} else {
			t.Fatal("concurrent reparent failed for an unrelated reason", err)
		}
	}
	var links, roots, violations int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE (id='left' AND parent_id='right') OR (id='right' AND parent_id='left')),
 (SELECT COUNT(*) FROM favorite_collections WHERE parent_id IS NULL),(SELECT COUNT(*) FROM pragma_foreign_key_check)`).Scan(&links, &roots, &violations); err != nil || success != 1 || rejected != 1 || links != 1 || roots != 1 || violations != 0 {
		t.Fatal("concurrent reparent admitted a cycle or lost a valid move", success, rejected, links, roots, violations, err)
	}
}

func TestFavoriteCollectionTreeMigrationRejectsInvalidOldTreesWithoutRepair(t *testing.T) {
	for _, scenario := range []struct{ name, corrupt, restore string }{
		{"cross-owner", `UPDATE favorite_collections SET parent_id='v-root' WHERE id='b-child'`, `UPDATE favorite_collections SET parent_id='a-root' WHERE id='b-child'`},
		{"cycle", `UPDATE favorite_collections SET parent_id='c-grand' WHERE id='a-root'`, `UPDATE favorite_collections SET parent_id=NULL WHERE id='a-root'`},
		{"self-cycle", `UPDATE favorite_collections SET parent_id='a-root' WHERE id='a-root'`, `UPDATE favorite_collections SET parent_id=NULL WHERE id='a-root'`},
		{"null-id", `INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES(NULL,'u','Invalid',0)`, `DELETE FROM favorite_collections WHERE id IS NULL`},
		{"nul-id", `INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES(CAST(X'610062' AS TEXT),'u','Invalid',0)`, `DELETE FROM favorite_collections WHERE id=CAST(X'610062' AS TEXT)`},
		{"oversize-id", `INSERT INTO favorite_collections(id,user_id,name,created_at) VALUES(printf('%065d',1),'u','Invalid',0)`, `DELETE FROM favorite_collections WHERE id=printf('%065d',1)`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			db := testDB(t)
			previous := beforeFavoriteCollectionTree(t)
			if err := Migrate(t.Context(), db, previous); err != nil {
				t.Fatal(err)
			}
			seedFavoriteCollectionTree(t, db)
			if _, err := db.Exec(scenario.corrupt); err != nil {
				t.Fatal("old schema did not permit the intended invalid fixture", err)
			}
			if err := Migrate(t.Context(), db, migrations.Files); err == nil {
				t.Fatal("invalid old hierarchy was silently accepted")
			}
			var columns, index, ledger, favorite int
			if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM pragma_table_info('favorite_collections') WHERE name IN ('version','updated_at')),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name='idx_collection_owner_parent_page'),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0028_favorite_collection_tree.sql'),
 (SELECT COUNT(*) FROM favorites WHERE user_id='u' AND album_id='a' AND collection_id='b-child' AND favorited=1 AND version=3)`).Scan(&columns, &index, &ledger, &favorite); err != nil || columns != 0 || index != 0 || ledger != 0 || favorite != 1 {
				t.Fatal("rejected old hierarchy left partial schema or changed favorite data", columns, index, ledger, favorite, err)
			}
			if err := VerifySchema(t.Context(), db, previous); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(scenario.restore); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(t.Context(), db, migrations.Files); err != nil {
				t.Fatal("restored isolated fixture could not migrate", err)
			}
		})
	}
}

func TestFavoriteCollectionTreeMigrationLateTriggerFailureRollsBackAndRetries(t *testing.T) {
	db := testDB(t)
	previous := beforeFavoriteCollectionTree(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	seedFavoriteCollectionTree(t, db)
	if _, err := db.Exec(`CREATE TRIGGER favorite_collection_name_guard BEFORE INSERT ON tags BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("late hierarchy trigger collision was ignored")
	}
	var columns, index, addedTriggers, ledger, favorite int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM pragma_table_info('favorite_collections') WHERE name IN ('version','updated_at')),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name='idx_collection_owner_parent_page'),
 (SELECT COUNT(*) FROM sqlite_schema WHERE name IN ('favorite_collection_insert_guard','favorite_collection_identity_guard','favorite_collection_parent_guard')),
 (SELECT COUNT(*) FROM schema_migrations WHERE version='0028_favorite_collection_tree.sql'),
 (SELECT COUNT(*) FROM favorites WHERE user_id='u' AND album_id='a' AND collection_id='b-child' AND favorited=1 AND version=3)`).Scan(&columns, &index, &addedTriggers, &ledger, &favorite); err != nil || columns != 0 || index != 0 || addedTriggers != 0 || ledger != 0 || favorite != 1 {
		t.Fatal("late hierarchy failure left partial schema or lost data", columns, index, addedTriggers, ledger, favorite, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER favorite_collection_name_guard`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("hierarchy migration did not retry after removing the fixture collision", err)
	}
}

func TestFavoriteCollectionTreeMigration100KUsesOwnerParentKeysets(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeFavoriteCollectionTree(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,scope_id,created_at,updated_at) VALUES('u','tree-scale','not-a-login-hash','user','scale-scope',0,0);
 WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<99999)
 INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at)
 SELECT printf('c%06d',i),'u',CASE WHEN i<100 THEN NULL ELSE printf('c%06d',(i-100)%100) END,printf('Name %06d',i),i FROM n;
 WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<1999)
 INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at)
 SELECT printf('z%06d',i),'u',CASE WHEN i=0 THEN 'c000000' ELSE printf('z%06d',i-1) END,printf('Deep %06d',i),i FROM n`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT id FROM favorite_collections WHERE user_id='u' AND parent_id IS NULL AND id>'c000020' ORDER BY id LIMIT 31`,
		`SELECT id FROM favorite_collections WHERE user_id='u' AND parent_id='c000050' AND id>'c050000' ORDER BY id LIMIT 31`,
	} {
		rows, err := db.Query(`EXPLAIN QUERY PLAN ` + query)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			fmt.Fprintln(&plan, detail)
		}
		err = rows.Err()
		rows.Close()
		if err != nil || !strings.Contains(plan.String(), "SEARCH") || !strings.Contains(plan.String(), "idx_collection_owner_parent_page") || strings.Contains(plan.String(), "TEMP B-TREE") || strings.Contains(query, "OFFSET") {
			t.Fatal("large owner/parent selection lost indexed keysets", query, plan.String(), err)
		}
		rows, err = db.Query(query)
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
		err = rows.Err()
		rows.Close()
		if err != nil || len(ids) != 31 || !slices.IsSorted(ids) {
			t.Fatal("large owner/parent page was not bounded and ordered", query, len(ids), err)
		}
	}
	namePlan, err := db.Query(`EXPLAIN QUERY PLAN SELECT id FROM favorite_collections WHERE user_id='u' AND COALESCE(parent_id,'')=COALESCE('c000050','') AND name='Name 050000'`)
	if err != nil {
		t.Fatal(err)
	}
	var names strings.Builder
	for namePlan.Next() {
		var id, parent, unused int
		var detail string
		if err := namePlan.Scan(&id, &parent, &unused, &detail); err != nil {
			namePlan.Close()
			t.Fatal(err)
		}
		fmt.Fprintln(&names, detail)
	}
	err = namePlan.Err()
	namePlan.Close()
	if err != nil || !strings.Contains(names.String(), "SEARCH") || !strings.Contains(names.String(), "uq_collection_user_parent_name") || strings.Contains(names.String(), "TEMP B-TREE") {
		t.Fatal("collection collision guard lost its scoped name index", names.String(), err)
	}
	if _, err := db.Exec(`UPDATE favorite_collections SET parent_id='z001999' WHERE id='c000000'`); err == nil {
		t.Fatal("a deep descendant was accepted as its root's parent")
	}
	var preserved, roots, violations int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorite_collections WHERE user_id='u' AND version=1 AND updated_at=created_at),(SELECT COUNT(*) FROM favorite_collections WHERE parent_id IS NULL),(SELECT COUNT(*) FROM pragma_foreign_key_check)`).Scan(&preserved, &roots, &violations); err != nil || preserved != 102000 || roots != 100 || violations != 0 {
		t.Fatal("large migration lost folders, roots, timestamps or FK integrity", preserved, roots, violations, err)
	}
}
