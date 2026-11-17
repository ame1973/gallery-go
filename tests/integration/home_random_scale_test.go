package integration

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
)

func TestRandomAlbums100KEnforcesVisibilityWithinMetadataDeadline(t *testing.T) {
	f := newCollectionFixture(t)
	if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('random-visible','Visible',0),('random-hidden','Hidden',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('random-source','random-visible','Visible','/never-open-random-visible','{}','unavailable',0),('random-other-source','random-hidden','Hidden','/never-open-random-hidden','{}','unavailable',0);
 INSERT INTO permission_groups(id,name,created_at) VALUES('random-group','Random readers',0);
 INSERT INTO library_access(library_id,group_id) VALUES('random-visible','random-group');
 WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<99999)
 INSERT INTO albums(id,library_id,source_id,source_name,access_restricted,created_at,updated_at)
 SELECT printf('random-%06d',i),CASE WHEN i%2=0 THEN 'random-visible' ELSE 'random-hidden' END,CASE WHEN i%2=0 THEN 'random-source' ELSE 'random-other-source' END,'Album',CASE WHEN i%10=0 THEN 1 ELSE 0 END,0,0 FROM n`); err != nil {
		t.Fatal(err)
	}
	check := func(viewer auth.Session, want, eligible int, visibleOnly bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		started := time.Now()
		items, err := f.service.RandomAlbums(ctx, viewer, access.MaxRandomAlbums)
		elapsed := time.Since(started)
		cancel()
		if err != nil || items == nil || len(items) != want || elapsed >= 10*time.Second {
			t.Fatal("100k random sample exceeded the metadata deadline or returned the wrong size", elapsed, len(items), err)
		}
		seen := map[string]bool{}
		for _, item := range items {
			if seen[item.ID] || !strings.HasPrefix(item.ID, "random-") {
				t.Fatal("sample repeated an album or escaped the test dataset", item.ID)
			}
			seen[item.ID] = true
			n, err := strconv.Atoi(strings.TrimPrefix(item.ID, "random-"))
			if err != nil || visibleOnly && (n%2 != 0 || n%10 == 0 || item.LibraryID != "random-visible") {
				t.Fatal("large random sample crossed a library or album restriction", item, err)
			}
			if item.MediaCount != 0 || item.CoverMediaID != nil {
				t.Fatal("empty large sample invented media or a cover", item)
			}
		}
		fmt.Printf("random-albums 100k eligible=%d sample=%d elapsed=%s\n", eligible, want, elapsed)
	}
	// Before the grant, all 100,000 records must be filtered out before LIMIT.
	check(f.other, 0, 0, false)
	if _, err := f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES(?,'random-group')`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	check(f.other, 100, 40000, true)
	check(f.owner, 100, 100000, false)
}
