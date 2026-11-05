package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func TestExactDuplicateIndexMigrationPreservesExistingRowsAndOwnership(t *testing.T) {
	db := testDB(t)
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "0014_" {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		previous[name] = &fstest.MapFile{Data: data}
	}
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('library','Migration fixture',0);
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES('album','library','Owned',0,0);
 INSERT INTO media(id,album_id,kind,file_name,current_revision_id,created_at,updated_at) VALUES('one','album','image','one.png','r-one',0,0),('two','album','image','two.png','r-two',0,0);
 INSERT INTO media_revisions VALUES('r-one','one',?,123,0,1,0),('r-two','two',?,123,0,1,0),('old-one','one',?,123,0,2,0)`, strings.Repeat("a", 64), strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var revisions, current int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM media_revisions),(SELECT COUNT(*) FROM media m JOIN media_revisions r ON r.id=m.current_revision_id AND r.media_id=m.id)`).Scan(&revisions, &current); err != nil || revisions != 3 || current != 2 {
		t.Fatal("index migration rewrote historical/current ownership", revisions, current, err)
	}
	var indexes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='idx_revision_exact_signature'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatal("duplicate lookup index absent", indexes, err)
	}
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT m.id FROM media_revisions r JOIN media m ON m.current_revision_id=r.id AND r.media_id=m.id WHERE r.source_sha256=? AND r.source_size=?`, strings.Repeat("a", 64), 123)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "idx_revision_exact_signature") {
		t.Fatal("signature/current-owner lookup did not use its covering index", plan.String())
	}
}
