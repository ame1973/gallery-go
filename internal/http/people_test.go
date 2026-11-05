package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gallerygo/internal/access"
)

func TestPeopleHTTPPagesUseCurrentVisibilityAndScopeCollectionBoundCursors(t *testing.T) {
	f := protectedMedia(t)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 105 {
		id := fmt.Sprintf("person-%03d", index)
		if _, err := tx.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES(?,'media-source',?,'Same creator',0)`, id, "private/"+id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,created_at,updated_at) VALUES(?,'media-library','media-source',?,'Album',0,0)`, "album-"+id, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v2/people"
	response := f.reader.request("GET", path, nil, "", "")
	page := decodeResponse[listPage[access.Person]](t, response, 200)
	if len(page.Items) != 30 || !page.HasMore || page.NextCursor == nil || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("person page was not bounded/private", page, response.Header())
	}
	first := decodeResponse[listPage[access.Person]](t, f.reader.request("GET", path+"?limit=100", nil, "", ""), 200)
	last := decodeResponse[listPage[access.Person]](t, f.reader.request("GET", path+"?limit=100&cursor="+*first.NextCursor, nil, "", ""), 200)
	if len(first.Items) != 100 || len(last.Items) != 5 || last.HasMore || last.NextCursor != nil || last.Items[4].ID != "person-104" {
		t.Fatal("person cursor lost/duplicated rows", first, last)
	}
	person := decodeResponse[access.Person](t, f.reader.request("GET", path+"/person-000", nil, "", ""), 200)
	if person.Name != "Same creator" || person.SourceID != "media-source" || person.AlbumCount != 1 || person.CoverMediaID != nil || person.Version != 1 {
		t.Fatal("person detail does not match authorized list projection", person)
	}
	for _, private := range []string{"relative_path", "root_path", "private/person", f.source, "marker"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatal("person API exposed private mapping/paths", private)
		}
	}
	if _, err := f.db.Exec(`UPDATE albums SET person_id='person-000' WHERE id LIKE 'album-person-%'`); err != nil {
		t.Fatal(err)
	}
	memberPath := path + "/person-000/albums"
	members := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", memberPath+"?limit=100", nil, "", ""), 200)
	if len(members.Items) != 100 || !members.HasMore || members.NextCursor == nil {
		t.Fatal("person album page is not bounded", members)
	}
	end := decodeResponse[listPage[access.Album]](t, f.reader.request("GET", memberPath+"?limit=100&cursor="+*members.NextCursor, nil, "", ""), 200)
	if len(end.Items) != 5 || end.HasMore || end.NextCursor != nil {
		t.Fatal("person album keyset lost rows", end)
	}
	for _, route := range []string{path + "/person-001/albums", "/api/v2/albums", memberPath} {
		expectCode(t, f.reader.request("GET", route+"?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	}
	expectCode(t, f.reader.request("GET", path+"?cursor="+*members.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	for _, route := range []string{path, memberPath} {
		for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "cursor=x&cursor=y"} {
			expectCode(t, f.reader.request("GET", route+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
		}
		expectCode(t, f.reader.request("GET", route+"?source_id=media-source", nil, "", ""), 400, "INVALID_FILTER")
	}
	expectCode(t, f.reader.request("GET", path+"/missing", nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, f.reader.request("GET", path+"/missing/albums", nil, "", ""), 404, "NOT_FOUND")
	if _, err := f.db.Exec(`UPDATE users SET scope_id='rotated-person-scope',version=version+1 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", path+"?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, f.reader.request("GET", memberPath+"?cursor="+*members.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
}

func TestPeopleHTTPRejectsAnonymousHiddenInactiveAndBrokenQueryContexts(t *testing.T) {
	f := protectedMedia(t)
	if _, err := f.db.Exec(`INSERT INTO people(id,source_id,relative_path,source_name,display_name,created_at) VALUES('visible-person','media-source','private/person','Folder label','Visible creator',0),('empty-person','media-source','private/empty','Empty',NULL,0);
 UPDATE albums SET person_id='visible-person' WHERE id='media-album';
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,access_restricted,created_at,updated_at) VALUES('hidden-person-album','media-library','media-source','visible-person','Hidden',1,0,0);
 INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('0-hidden-person-cover','hidden-person-album','image','hidden.jpg',0,0);
 UPDATE albums SET cover_media_id='0-hidden-person-cover' WHERE id='hidden-person-album'`); err != nil {
		t.Fatal(err)
	}
	for _, client := range []adminClient{{handler: f.http}, {handler: f.http, cookie: &http.Cookie{Name: sessionCookie, Value: strings.Repeat("x", 43)}}} {
		for _, path := range []string{"/api/v2/people", "/api/v2/people/visible-person", "/api/v2/people/visible-person/albums"} {
			expectCode(t, client.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
		}
	}
	person := decodeResponse[access.Person](t, f.reader.request("GET", "/api/v2/people/visible-person", nil, "", ""), 200)
	if person.AlbumCount != 1 || person.CoverMediaID == nil || *person.CoverMediaID != "protected" || person.Name != "Visible creator" {
		t.Fatal("hidden albums/media affected public person aggregates", person)
	}
	adminPerson := decodeResponse[access.Person](t, f.admin.request("GET", "/api/v2/people/visible-person", nil, "", ""), 200)
	if adminPerson.AlbumCount != 2 || adminPerson.CoverMediaID == nil || *adminPerson.CoverMediaID != "0-hidden-person-cover" {
		t.Fatal("administrator person projection lost valid indexed albums", adminPerson)
	}
	for _, path := range []string{"/api/v2/people/empty-person", "/api/v2/people/empty-person/albums"} {
		expectCode(t, f.reader.request("GET", path, nil, "", ""), 404, "NOT_FOUND")
	}
	if _, err := f.db.Exec(`UPDATE libraries SET is_active=0 WHERE id='media-library'`); err != nil {
		t.Fatal(err)
	}
	for _, client := range []adminClient{f.reader, f.admin} {
		page := decodeResponse[listPage[access.Person]](t, client.request("GET", "/api/v2/people", nil, "", ""), 200)
		if page.Items == nil || len(page.Items) != 0 || page.HasMore || page.NextCursor != nil {
			t.Fatal("inactive library disclosed creator existence", page)
		}
		expectCode(t, client.request("GET", "/api/v2/people/visible-person", nil, "", ""), 404, "NOT_FOUND")
	}
	if _, err := f.db.Exec(`UPDATE libraries SET is_active=1; ALTER TABLE people RENAME TO private_broken_people`); err != nil {
		t.Fatal(err)
	}
	failed := f.reader.request("GET", "/api/v2/people", nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(failed.Body.String(), "people") || strings.Contains(failed.Body.String(), "SELECT") || strings.Contains(failed.Body.String(), "/private") {
		t.Fatal("person query failure exposed internal schema/path", failed.Body.String())
	}
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, f.reader.request("GET", "/api/v2/people", nil, "", ""), 401, "UNAUTHENTICATED")
}
