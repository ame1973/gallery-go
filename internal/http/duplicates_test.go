package httpapi

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"gallerygo/internal/media"
)

func duplicateHTTPSource(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('duplicate-library','Duplicates',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES
 ('duplicate-source','duplicate-library','one','/private/never-open-duplicate-source','{}','unavailable',0),
 ('duplicate-other-source','duplicate-library','two','/private/never-open-other-source','{}','unavailable',0);
 INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES
 ('duplicate-album','duplicate-library','duplicate-source','One',0,0),
 ('duplicate-other-album','duplicate-library','duplicate-other-source','Two',0,0)`); err != nil {
		t.Fatal(err)
	}
}

func duplicateHTTPRow(t *testing.T, tx *sql.Tx, id, revision, hash, source, album string) {
	t.Helper()
	if _, err := tx.Exec(`INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,sha256,updated_at) VALUES(?,?,?,123,0,?,0)`, "entry-"+id, source, "private/"+id+".jpg", hash); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO media(id,album_id,source_entry_id,kind,file_name,current_revision_id,created_at,updated_at) VALUES(?,?,?,'image',?,?,0,0)`, id, album, "entry-"+id, "/private/basename/"+id+".jpg", revision); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO media_revisions VALUES(?,?,?,123,0,1,0)`, revision, id, hash); err != nil {
		t.Fatal(err)
	}
}

func TestExactDuplicateHTTPPagesBindScopeFiltersAndImmutableGroup(t *testing.T) {
	db, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	duplicateHTTPSource(t, db)
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := range 105 {
		for copy := range 2 {
			id := fmt.Sprintf("group-%03d-%d", index, copy)
			duplicateHTTPRow(t, tx, id, "revision-"+id, fmt.Sprintf("%064x", index+1), "duplicate-source", "duplicate-album")
		}
	}
	for index := range 105 {
		id := fmt.Sprintf("big-%03d", index)
		duplicateHTTPRow(t, tx, id, "revision-"+id, strings.Repeat("b", 64), "duplicate-source", "duplicate-album")
	}
	duplicateHTTPRow(t, tx, "other-copy", "revision-other-copy", strings.Repeat("b", 64), "duplicate-other-source", "duplicate-other-album")
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	const path = "/api/v2/admin/duplicates/exact"
	response := admin.request("GET", path, nil, "", "")
	page := decodeResponse[listPage[media.ExactDuplicateGroup]](t, response, 200)
	if len(page.Items) != 30 || !page.HasMore || page.NextCursor == nil || page.Items[0].ID != "revision-big-000" || page.Items[0].MediaCount != 106 || response.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal("duplicate group page is not bounded/current/private", page, response.Header())
	}
	first := decodeResponse[listPage[media.ExactDuplicateGroup]](t, admin.request("GET", path+"?limit=100", nil, "", ""), 200)
	last := decodeResponse[listPage[media.ExactDuplicateGroup]](t, admin.request("GET", path+"?limit=100&cursor="+*first.NextCursor, nil, "", ""), 200)
	if len(first.Items) != 100 || len(last.Items) != 6 || last.HasMore || last.NextCursor != nil {
		t.Fatal("group keyset lost rows/counts", first, last)
	}
	group := path + "/" + page.Items[0].ID + "/media"
	membersResponse := admin.request("GET", group+"?limit=100", nil, "", "")
	members := decodeResponse[listPage[media.Metadata]](t, membersResponse, 200)
	if len(members.Items) != 100 || !members.HasMore || members.NextCursor == nil || members.Items[0].FileName != "big-000.jpg" {
		t.Fatal("member metadata page is not bounded/sanitized", members)
	}
	end := decodeResponse[listPage[media.Metadata]](t, admin.request("GET", group+"?limit=100&cursor="+*members.NextCursor, nil, "", ""), 200)
	if len(end.Items) != 6 || end.HasMore || end.NextCursor != nil {
		t.Fatal("member cursor lost rows", end)
	}
	for _, private := range []string{"/private", strings.Repeat("b", 64), "source_sha256", "root_path", "relative_path", "marker", "storage_path"} {
		if strings.Contains(membersResponse.Body.String(), private) || strings.Contains(response.Body.String(), private) {
			t.Fatal("duplicate projection leaked private data", private)
		}
	}
	filtered := decodeResponse[listPage[media.ExactDuplicateGroup]](t, admin.request("GET", path+"?source_id=duplicate-source&limit=1", nil, "", ""), 200)
	if len(filtered.Items) != 1 || filtered.Items[0].MediaCount != 105 || filtered.NextCursor == nil {
		t.Fatal("filters did not precede group counts", filtered)
	}
	for _, other := range []string{path + "?library_id=duplicate-library&", group + "?", "/api/v2/admin/jobs?"} {
		expectCode(t, admin.request("GET", other+"cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	}
	expectCode(t, admin.request("GET", path+"?cursor="+*filtered.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, admin.request("GET", path+"/revision-group-000-0/media?cursor="+*members.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	for _, query := range []string{"limit=0", "limit=101", "limit=1&limit=2", "cursor=x&cursor=y"} {
		expectCode(t, admin.request("GET", path+"?"+query, nil, "", ""), 400, "INVALID_PAGINATION")
	}
	for _, query := range []string{"perceptual=true", "source_sha256=secret", "source_id=", "source_id=x&source_id=y", "library_id=" + url.QueryEscape(strings.Repeat("a", 65)), "source_id=%2Fprivate", "library_id=padded%20", "source_id=bad%00"} {
		for _, route := range []string{path, group} {
			expectCode(t, admin.request("GET", route+"?"+query, nil, "", ""), 400, "INVALID_FILTER")
		}
	}
	expectCode(t, admin.request("GET", path+"?cursor=invalid", nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, admin.request("GET", path+"/missing/media", nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, admin.request("GET", path+"/revision-big-001/media", nil, "", ""), 404, "NOT_FOUND")
	expectCode(t, admin.request("GET", group+"?source_id=duplicate-other-source", nil, "", ""), 404, "NOT_FOUND")
	if _, err := db.Exec(`UPDATE users SET scope_id='rotated-duplicate-scope',version=version+1 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", path+"?cursor="+*page.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	expectCode(t, admin.request("GET", group+"?cursor="+*members.NextCursor, nil, "", ""), 400, "INVALID_CURSOR")
	if _, err := db.Exec(`INSERT INTO media_revisions VALUES('new-big-revision','big-000',?,999,0,2,0); UPDATE media SET current_revision_id='new-big-revision' WHERE id='big-000'`, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", group, nil, "", ""), 404, "NOT_FOUND")
}

func TestExactDuplicateHTTPRejectsAnonymousNonAdminAndSanitizesReadFailures(t *testing.T) {
	db, h := authFixture(t)
	admin, owner := ownerClient(t, h)
	viewerUser := createHTTPUser(t, admin, "duplicate-reader", "user")
	viewer := accountClient(t, h, viewerUser.Username, testPassword)
	const path = "/api/v2/admin/duplicates/exact"
	for _, test := range []struct {
		client adminClient
		status int
		code   string
	}{
		{adminClient{handler: h}, 401, "UNAUTHENTICATED"},
		{adminClient{handler: h, cookie: &http.Cookie{Name: sessionCookie, Value: strings.Repeat("x", 43)}}, 401, "UNAUTHENTICATED"},
		{viewer, 403, "ADMIN_REQUIRED"},
	} {
		for _, route := range []string{path, path + "/missing/media"} {
			expectCode(t, test.client.request("GET", route, nil, "", ""), test.status, test.code)
		}
	}
	empty := decodeResponse[listPage[media.ExactDuplicateGroup]](t, admin.request("GET", path, nil, "", ""), 200)
	if empty.Items == nil || len(empty.Items) != 0 || empty.HasMore || empty.NextCursor != nil {
		t.Fatal("empty duplicate corpus did not return an explicit empty page", empty)
	}
	if _, err := db.Exec(`DROP TABLE media_revisions`); err != nil {
		t.Fatal(err)
	}
	failed := admin.request("GET", path, nil, "", "")
	expectCode(t, failed, 500, "INTERNAL_ERROR")
	if strings.Contains(problemContent(t, failed), "media_revisions") || strings.Contains(problemContent(t, failed), "SELECT") || strings.Contains(problemContent(t, failed), "/private") {
		t.Fatal("duplicate SQL failure leaked private implementation", failed.Body.String())
	}
	if _, err := db.Exec(`UPDATE users SET active=0 WHERE id=?`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, admin.request("GET", path, nil, "", ""), 401, "UNAUTHENTICATED")
}
