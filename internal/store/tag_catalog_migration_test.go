package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeTagCatalog(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "0015_" {
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

func TestTagCatalogMigrationPreservesExactLabelsAndAssociations(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeTagCatalog(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('library','Migration',0);
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES('album','library','Owned',0,0);
 INSERT INTO tags(id,name) VALUES('legacy','旅行'),('upper','Travel'),('lower','travel');
 INSERT INTO album_tags(album_id,tag_id) VALUES('album','legacy')`); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("pending tag catalog migration was accepted")
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var count, version, links int
	var name string
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM tags),version,name,(SELECT COUNT(*) FROM album_tags WHERE album_id='album' AND tag_id='legacy') FROM tags WHERE id='legacy'`).Scan(&count, &version, &name, &links); err != nil || count != 3 || version != 1 || name != "旅行" || links != 1 {
		t.Fatal("migration rewrote tag labels/identities or removed album edges", count, version, name, links, err)
	}
	if _, err := db.Exec(`INSERT INTO tags(id,name) VALUES('duplicate','Travel')`); err == nil {
		t.Fatal("migration removed exact label uniqueness")
	}
	if _, err := db.Exec(`INSERT INTO tags(id,name) VALUES('case-variant','TRAVEL')`); err != nil {
		t.Fatal("migration silently case-folded binary label identity", err)
	}
	for _, invalid := range []int{0, -1} {
		if _, err := db.Exec(`UPDATE tags SET version=? WHERE id='legacy'`, invalid); err == nil {
			t.Fatal("nonpositive tag version was accepted", invalid)
		}
	}
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT EXISTS(SELECT 1 FROM album_tags WHERE tag_id=?)`, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
	}
	if err := rows.Err(); err != nil || !strings.Contains(plan.String(), "idx_album_tag_reverse") {
		t.Fatal("assigned-tag guard does not use the reverse edge index", plan.String(), err)
	}
}

func TestTagCatalogMigrationFailureRollsBackSchemaAndLedger(t *testing.T) {
	db := testDB(t)
	previous := beforeTagCatalog(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tags(id,name) VALUES('preserved','Exact label'); CREATE INDEX idx_album_tag_reverse ON tags(name)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("injected late index failure was ignored")
	}
	var columns, ledger, tags int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM pragma_table_info('tags') WHERE name='version'),(SELECT COUNT(*) FROM schema_migrations WHERE version='0015_tag_catalog.sql'),(SELECT COUNT(*) FROM tags WHERE id='preserved' AND name='Exact label')`).Scan(&columns, &ledger, &tags); err != nil || columns != 0 || ledger != 0 || tags != 1 {
		t.Fatal("failed migration left a partial schema/ledger or lost old tags", columns, ledger, tags, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal("old schema history was not preserved after rollback", err)
	}
	if _, err := db.Exec(`DROP INDEX idx_album_tag_reverse`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("clean retry after removing the injected failure did not apply", err)
	}
}
