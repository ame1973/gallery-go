package integration

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/access"
)

func TestFavoriteSubtree5KUsesIndexedBoundedPages(t *testing.T) {
	testFavoriteSubtreeScale(t, 5_000, 50)
}

func TestFavoriteSubtree100KUsesIndexedBoundedPages(t *testing.T) {
	testFavoriteSubtreeScale(t, 100_000, 1_000)
}

func testFavoriteSubtreeScale(t *testing.T, total, interval int) {
	t.Helper()
	f := newCollectionFixture(t)
	if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('subtree-scale-library','Library',0);
 INSERT INTO permission_groups(id,name,created_at) VALUES('subtree-scale-group','Readers',0);
 INSERT INTO library_access(library_id,group_id) VALUES('subtree-scale-library','subtree-scale-group');
 INSERT INTO tags(id,name) VALUES('subtree-scale-tag','Tag')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at,updated_at) VALUES
 ('subtree-scale-root',?,NULL,'Root',0,0),('subtree-scale-child',?,'subtree-scale-root','Child',0,0),
 ('subtree-scale-grandchild',?,'subtree-scale-child','Grandchild',0,0),
 ('subtree-scale-unrelated',?,NULL,'Unrelated',0,0),('subtree-scale-foreign',?,NULL,'Foreign',0,0)`,
		f.other.User.ID, f.other.User.ID, f.other.User.ID, f.other.User.ID, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	depth, unrelated := 80, 500
	if total == 100_000 {
		depth, unrelated = 2_000, 10_000
	}
	if _, err := f.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<?)
 INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at,updated_at)
 SELECT printf('subtree-chain-%04d',i),?,CASE WHEN i=0 THEN 'subtree-scale-root' ELSE printf('subtree-chain-%04d',i-1) END,printf('Chain %04d',i),0,0 FROM n`, depth-1, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<?)
 INSERT INTO favorite_collections(id,user_id,name,created_at,updated_at)
 SELECT printf('subtree-unrelated-%05d',i),?,printf('Unrelated %05d',i),0,0 FROM n`, unrelated-1, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<?)
 INSERT INTO albums(id,library_id,source_name,access_restricted,created_at,updated_at)
 SELECT printf('subtree-%06d',i),'subtree-scale-library',printf('Name %02d',i%17),i%7=0,i%11,0 FROM n`, total-1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO album_tags(album_id,tag_id)
 SELECT id,'subtree-scale-tag' FROM albums WHERE CAST(substr(id,9) AS INTEGER)%3=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorites(user_id,album_id,collection_id,created_at,favorited,updated_at,version)
 SELECT ?,id,CASE WHEN CAST(substr(id,9) AS INTEGER)=? THEN ? WHEN CAST(substr(id,9) AS INTEGER)%?=0
 THEN CASE WHEN CAST(substr(id,9) AS INTEGER)/?%2=0 THEN 'subtree-scale-root' ELSE 'subtree-scale-grandchild' END
 ELSE 'subtree-scale-unrelated' END,0,1,0,1 FROM albums WHERE library_id='subtree-scale-library'`,
		f.other.User.ID, interval, fmt.Sprintf("subtree-chain-%04d", depth-1), interval, interval); err != nil {
		t.Fatal(err)
	}
	var albums, favorites int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM albums WHERE library_id='subtree-scale-library'),
 (SELECT COUNT(*) FROM favorites WHERE user_id=?)`, f.other.User.ID).Scan(&albums, &favorites); err != nil || albums != total || favorites != total {
		t.Fatalf("fixture has %d albums and %d favorites, want %d: %v", albums, favorites, total, err)
	}
	filter := access.AlbumFilter{FavoriteCollectionID: "subtree-scale-root", FavoriteIncludeDescendants: true}
	queryPage := func(filter access.AlbumFilter) []access.Album {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		started := time.Now()
		items, err := f.service.Albums(ctx, f.other, filter, access.AlbumPosition{}, 11)
		if err != nil {
			t.Fatalf("subtree selection %v failed after %s: %v", filter, time.Since(started), err)
		}
		t.Logf("subtree selection size=%d sort=%q tag=%q completed in %s", total, filter.Sort, filter.TagID, time.Since(started))
		return items
	}
	if items := queryPage(filter); items == nil || len(items) != 0 {
		t.Fatal("subtree selection granted an unauthorized library", items)
	}
	if _, err := f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES(?,'subtree-scale-group')`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"", "latest", "oldest", "name_asc", "name_desc", "top_rated", "popular"} {
		for _, tagged := range []bool{false, true} {
			filter.Sort, filter.TagID = order, ""
			if tagged {
				filter.TagID = "subtree-scale-tag"
			}
			query, args := access.AlbumSelectionQuery(f.other.User.ID, filter, access.AlbumPosition{}, int64(0), 11)
			rows, err := f.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query, args...)
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
			if err != nil || !strings.Contains(plan.String(), "idx_collection_owner_parent_page") || strings.Contains(plan.String(), "TEMP B-TREE") || strings.Contains(query, "OFFSET") {
				t.Fatal("subtree selection lost indexed traversal or ordered keysets", filter, plan.String(), err)
			}
			if order == "" && !tagged && !strings.Contains(plan.String(), "idx_favorites_active_page") {
				t.Fatal("subtree ID pages lost the actor/album order index", plan.String())
			}
			if tagged && order == "" && !strings.Contains(plan.String(), "idx_album_tag_reverse") {
				t.Fatal("subtree tag pages lost the reverse tag index", plan.String())
			}
			if order != "" || tagged {
				pointProbe := false
				for _, line := range strings.Split(plan.String(), "\n") {
					pointProbe = pointProbe || strings.Contains(line, "SEARCH f ") && strings.Contains(line, "idx_favorites_active_page") && strings.Contains(line, "user_id=?") && strings.Contains(line, "album_id=?")
				}
				if !pointProbe {
					t.Fatal("subtree selection lost its actor/album point probe", filter, plan.String())
				}
			}
			var expected []int
			for i := 0; i < total; i += interval {
				if i%7 != 0 && (!tagged || i%3 == 0) {
					expected = append(expected, i)
				}
			}
			slices.SortFunc(expected, func(a, b int) int {
				compare := func(a, b int) int { return a - b }
				switch order {
				case "latest", "oldest":
					if n := compare(a%11, b%11); n != 0 {
						if order == "latest" {
							return -n
						}
						return n
					}
				case "name_asc", "name_desc":
					if n := compare(a%17, b%17); n != 0 {
						if order == "name_desc" {
							return -n
						}
						return n
					}
				}
				if order == "latest" || order == "name_desc" || order == "top_rated" || order == "popular" {
					return compare(b, a)
				}
				return compare(a, b)
			})
			items := queryPage(filter)
			if len(expected) < 11 || len(items) != 11 {
				t.Fatal("subtree selection did not return its bounded page", filter, len(expected), len(items))
			}
			for i, item := range items {
				number, err := strconv.Atoi(strings.TrimPrefix(item.ID, "subtree-"))
				if err != nil || number != expected[i] {
					t.Fatal("subtree selection lost membership, visibility or ordering", filter, i, item.ID, expected[i], err)
				}
			}
		}
	}
	filter = access.AlbumFilter{FavoriteCollectionID: "subtree-scale-foreign", FavoriteIncludeDescendants: true}
	if items := queryPage(filter); items == nil || len(items) != 0 {
		t.Fatal("subtree selection disclosed a different account's folder", items)
	}
	if _, err := f.db.Exec(`DELETE FROM user_groups WHERE user_id=?`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	filter.FavoriteCollectionID = "subtree-scale-root"
	if items := queryPage(filter); items == nil || len(items) != 0 {
		t.Fatal("subtree selection retained a revoked library grant", items)
	}
}
