package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"gallerygo/migrations"
)

func beforeAlbumCreators(t *testing.T) fstest.MapFS {
	t.Helper()
	previous := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if name >= "0016_" {
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

func TestAlbumCreatorMigrationPreservesMappingIdentityAndInteractions(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, beforeAlbumCreators(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('l','Library',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,created_at) VALUES('s','l','Source','/never-open-creators','{}',0);
 INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('p','s','Creator','Creator',0);
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,display_name,version,access_restricted,created_at,updated_at) VALUES('a','l','s','p','Album','Override',7,1,0,0),('empty','l','s',NULL,'Empty',NULL,1,0,0,0);
 INSERT INTO tags(id,name) VALUES('tag','Retained'); INSERT INTO album_tags(album_id,tag_id) VALUES('a','tag')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := VerifySchema(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var person, mapped, mode, label string
	var override *string
	var version, links int
	var restricted bool
	if err := db.QueryRow(`SELECT person_id,mapped_person_id,person_mode,person_override_id,display_name,version,access_restricted,(SELECT COUNT(*) FROM album_tags WHERE album_id='a') FROM albums WHERE id='a'`).Scan(&person, &mapped, &mode, &override, &label, &version, &restricted, &links); err != nil || person != "p" || mapped != "p" || mode != "mapped" || override != nil || label != "Override" || version != 7 || !restricted || links != 1 {
		t.Fatal("creator migration rewrote classification/overrides/policy/interactions", person, mapped, mode, override, label, version, restricted, links, err)
	}
	var empty bool
	if err := db.QueryRow(`SELECT person_id IS NULL AND mapped_person_id IS NULL AND person_mode='mapped' AND person_override_id IS NULL FROM albums WHERE id='empty'`).Scan(&empty); err != nil || !empty {
		t.Fatal("unclassified album was guessed during backfill", empty, err)
	}
	for _, query := range []string{
		`UPDATE albums SET person_mode='unknown' WHERE id='a'`,
		`UPDATE albums SET person_mode='person' WHERE id='a'`,
		`UPDATE albums SET person_override_id='p' WHERE id='a'`,
		`UPDATE albums SET person_mode='person',person_override_id='missing' WHERE id='a'`,
		`UPDATE albums SET mapped_person_id='missing' WHERE id='a'`,
	} {
		if _, err := db.Exec(query); err == nil {
			t.Fatal("invalid mode/override/foreign key was accepted", query)
		}
	}
	if _, err := db.Exec(`UPDATE albums SET person_mode='person',person_override_id='p' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE albums SET person_mode='none',person_override_id=NULL,person_id=NULL WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	var plan string
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM albums WHERE mapped_person_id=?`, "p")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if err := rows.Err(); err != nil || !strings.Contains(plan, "idx_album_mapped_person") {
		t.Fatal("mapped creator provenance lookup lost its index", plan, err)
	}
}

func TestAlbumCreatorMigrationFailureRollsBackProvenanceAndLedger(t *testing.T) {
	db := testDB(t)
	previous := beforeAlbumCreators(t)
	if err := Migrate(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('l','Library',0);
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES('a','l','Preserved',0,0);
 CREATE INDEX idx_album_mapped_person ON albums(person_id)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err == nil {
		t.Fatal("injected late creator-index failure was ignored")
	}
	var columns, ledger, albums int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM pragma_table_info('albums') WHERE name IN ('mapped_person_id','person_mode','person_override_id')),(SELECT COUNT(*) FROM schema_migrations WHERE version='0016_album_creators.sql'),(SELECT COUNT(*) FROM albums WHERE id='a' AND source_name='Preserved')`).Scan(&columns, &ledger, &albums); err != nil || columns != 0 || ledger != 0 || albums != 1 {
		t.Fatal("failed creator migration left partial schema/ledger/data", columns, ledger, albums, err)
	}
	if err := VerifySchema(t.Context(), db, previous); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX idx_album_mapped_person`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal("creator migration could not retry after rollback", err)
	}
}
