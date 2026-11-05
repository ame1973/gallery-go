package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"gallerygo/internal/auth"
)

type personFixture struct {
	svc               *Service
	owner, base, both auth.Session
	privateGroup      Group
}

func indexedPeopleFixture(t *testing.T) personFixture {
	t.Helper()
	s, owner := fixture(t)
	baseGroup, privateGroup := group(t, s, owner, "Person library"), group(t, s, owner, "Person private")
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('person-library','One',0),('other-person-library','Other',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('person-source','person-library','one','/private/never-open-person-source','{}','unavailable',0),('other-person-source','other-person-library','other','/private/never-open-other-person-source','{}','healthy',0);
 INSERT INTO people(id,source_id,relative_path,source_name,display_name,created_at) VALUES
 ('person-one','person-source','private/first/Creator','Same label','Visible alias',0),
 ('person-two','person-source','private/second/Creator','Same label',NULL,0),
 ('person-hidden','other-person-source','private/foreign/Creator','Hidden creator',NULL,0),
 ('person-empty','person-source','private/empty','Empty',NULL,0),
 ('person-no-path','person-source',NULL,'Not indexed',NULL,0),
 ('person-root-path','person-source','.','Not mapped',NULL,0),
 ('person-unclassified',NULL,NULL,'Unclassified',NULL,0);
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,cover_media_id,created_at,updated_at) VALUES
 ('person-open','person-library','person-source','person-one','Open','a-secret-media',0,0),
 ('person-private','person-library','person-source','person-one','Private','a-secret-media',0,0),
 ('person-other','person-library','person-source','person-two','Other',NULL,0,0),
 ('person-foreign','other-person-library','other-person-source','person-hidden','Foreign',NULL,0,0),
 ('person-cross-link','person-library','person-source','person-hidden','Cross link',NULL,0,0),
 ('person-no-path-album','person-library','person-source','person-no-path','Not indexed',NULL,0,0),
 ('person-root-path-album','person-library','person-source','person-root-path','Not mapped',NULL,0,0),
 ('person-unclassified-album','person-library','person-source','person-unclassified','Unclassified',NULL,0,0);
 INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES
 ('z-visible-media','person-open','image','visible.jpg',0,0),
 ('a-secret-media','person-private','image','secret.jpg',0,0),
 ('archived-cover','person-open','image','archived.jpg',0,0);
 UPDATE media SET availability='archived' WHERE id='archived-cover';
 INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,updated_at) VALUES('wrong-person-entry','other-person-source','never-read.jpg',1,0,0);
 INSERT INTO media(id,album_id,source_entry_id,kind,file_name,created_at,updated_at) VALUES('0-wrong-source-media','person-open','wrong-person-entry','image','wrong.jpg',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLibraryPolicy(t.Context(), owner, "person-library", 1, []string{baseGroup.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetAlbumPolicy(t.Context(), owner, "person-private", 1, true, []string{privateGroup.ID}); err != nil {
		t.Fatal(err)
	}
	base, both := reader(t, s, "person-base"), reader(t, s, "person-both")
	membership(t, s, owner, base, baseGroup.ID)
	membership(t, s, owner, both, baseGroup.ID, privateGroup.ID)
	return personFixture{s, owner, base, both, privateGroup}
}

func TestPeopleListsDetailsCountsCoversAndAlbumsShareFreshSQLVisibility(t *testing.T) {
	f := indexedPeopleFixture(t)
	for _, test := range []struct {
		viewer auth.Session
		count  int64
		cover  string
		total  int
	}{{f.base, 1, "z-visible-media", 2}, {f.both, 2, "a-secret-media", 2}, {f.owner, 2, "a-secret-media", 3}} {
		people, err := f.svc.People(t.Context(), test.viewer, "", 30)
		if err != nil || len(people) != test.total {
			t.Fatal("person name visibility differs from album access", test, people, err)
		}
		person, err := f.svc.Person(t.Context(), test.viewer, "person-one")
		if err != nil || person.ID != "person-one" || person.Name != "Visible alias" || person.SourceID != "person-source" || person.AlbumCount != test.count || person.CoverMediaID == nil || *person.CoverMediaID != test.cover || person.Version != 1 {
			t.Fatal("person aggregates/cover leaked a hidden or foreign resource", test, person, err)
		}
		albums, err := f.svc.PersonAlbums(t.Context(), test.viewer, "person-one", "", 30)
		if err != nil || int64(len(albums)) != test.count {
			t.Fatal("person album list differs from its visible count", albums, person, err)
		}
		for _, listed := range people {
			detail, err := f.svc.Person(t.Context(), test.viewer, listed.ID)
			if err != nil || detail.ID != listed.ID || detail.Name != listed.Name || detail.AlbumCount != listed.AlbumCount {
				t.Fatal("person list/detail predicate mismatch", listed, detail, err)
			}
		}
		encoded, _ := json.Marshal(people)
		for _, private := range []string{"relative_path", "root_path", "/private", "private/first", "private/second", "marker"} {
			if strings.Contains(string(encoded), private) {
				t.Fatal("person DTO exposed private source identity/path", private, string(encoded))
			}
		}
	}
	for _, id := range []string{"person-empty", "person-no-path", "person-root-path", "person-unclassified", "missing"} {
		for _, viewer := range []auth.Session{f.owner, f.base} {
			if _, err := f.svc.Person(t.Context(), viewer, id); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("unbound/empty/unknown person became public", id, err)
			}
			if _, err := f.svc.PersonAlbums(t.Context(), viewer, id, "", 30); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("hidden person's albums revealed existence", id, err)
			}
		}
	}
	if _, err := f.svc.Person(t.Context(), f.base, "person-hidden"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("cross-source album association revealed another creator", err)
	}
	second, err := f.svc.Person(t.Context(), f.base, "person-two")
	if err != nil || second.Name != "Same label" || second.CoverMediaID != nil || second.AlbumCount != 1 {
		t.Fatal("empty visible album has no valid nullable cover", second, err)
	}
}

func TestPeopleKeepSourcePathIdentitiesAndRecheckRevokedMemberships(t *testing.T) {
	f := indexedPeopleFixture(t)
	if err := f.svc.DeleteGroup(t.Context(), f.owner, f.privateGroup.ID, f.privateGroup.Version); err != nil {
		t.Fatal(err)
	}
	person, err := f.svc.Person(t.Context(), f.both, "person-one")
	if err != nil || person.AlbumCount != 1 || person.CoverMediaID == nil || *person.CoverMediaID != "z-visible-media" {
		t.Fatal("deleted private group broadened creator aggregates", person, err)
	}
	var restricted bool
	if err := f.svc.db.QueryRow(`SELECT access_restricted FROM albums WHERE id='person-private'`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("creator read cleared persistent album restriction", restricted, err)
	}
	if _, err := f.svc.db.Exec(`UPDATE people SET display_name=NULL WHERE id='person-one'`); err != nil {
		t.Fatal(err)
	}
	people, err := f.svc.People(t.Context(), f.base, "", 30)
	if err != nil || len(people) != 2 || people[0].Name != "Same label" || people[1].Name != "Same label" || people[0].ID == people[1].ID {
		t.Fatal("equal source labels collapsed separate full-path identities", people, err)
	}
	for _, statement := range []string{
		`UPDATE sources SET is_active=0 WHERE id='person-source'`,
		`UPDATE sources SET is_active=1; UPDATE libraries SET is_active=0 WHERE id='person-library'`,
		`UPDATE libraries SET is_active=1; UPDATE sources SET library_id='other-person-library' WHERE id='person-source'`,
	} {
		if _, err := f.svc.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		for _, viewer := range []auth.Session{f.base, f.owner} {
			if _, err := f.svc.Person(t.Context(), viewer, "person-one"); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("inactive/reassigned source bypassed person visibility", statement, err)
			}
		}
	}
}

func TestPeopleAndPersonAlbumsUseBoundedStableIDKeysets(t *testing.T) {
	s, owner := fixture(t)
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('page-library','Pages',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,created_at) VALUES('page-source','page-library','pages','/never-open-pages','{}',0)`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 105 {
		id := fmt.Sprintf("person-%03d", index)
		if _, err := tx.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES(?,'page-source',?,'Same',0)`, id, "private/"+id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES(?,'page-library','page-source',?,'Album',0,0)`, "album-"+id, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	first, err := s.People(t.Context(), owner, "", 100)
	if err != nil || len(first) != 100 {
		t.Fatal("person keyset is not bounded", first, err)
	}
	last, err := s.People(t.Context(), owner, first[99].ID, 100)
	if err != nil || len(last) != 5 || last[0].ID != "person-100" || last[4].ID != "person-104" {
		t.Fatal("person keyset lost/duplicated IDs", last, err)
	}
	if _, err := s.db.Exec(`UPDATE albums SET person_id='person-000'`); err != nil {
		t.Fatal(err)
	}
	firstAlbums, err := s.PersonAlbums(t.Context(), owner, "person-000", "", 100)
	if err != nil || len(firstAlbums) != 100 {
		t.Fatal("person albums page is not bounded", firstAlbums, err)
	}
	lastAlbums, err := s.PersonAlbums(t.Context(), owner, "person-000", firstAlbums[99].ID, 100)
	if err != nil || len(lastAlbums) != 5 {
		t.Fatal("person album cursor lost rows", lastAlbums, err)
	}
	for _, limit := range []int{0, 102} {
		if _, err := s.People(t.Context(), owner, "", limit); !errors.Is(err, ErrGroupSet) {
			t.Fatal("invalid person page limit accepted", err)
		}
		if _, err := s.PersonAlbums(t.Context(), owner, "person-000", "", limit); !errors.Is(err, ErrGroupSet) {
			t.Fatal("invalid person album limit accepted", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.People(ctx, owner, "", 30); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled person query was not stopped", err)
	}
}
