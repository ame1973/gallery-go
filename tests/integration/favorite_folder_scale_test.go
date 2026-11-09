package integration

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"gallerygo/internal/access"
)

// Query plans and bounded cards share the exact production SQL builder. This
// independent binary keeps the complete 100k matrix within the race deadline.
func TestAlbumSelection100KUsesExistingSelectiveIndexesBeforeBoundedCards(t *testing.T) {
	f := newCollectionFixture(t)
	if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('scale-l1','One',0),('scale-l2','Two',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('scale-s1','scale-l1','One','/never-open-folder-scale-one','{}','unavailable',0),('scale-s2','scale-l2','Two','/never-open-folder-scale-two','{}','healthy',0);
 INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('scale-p1','scale-s1','one','Same',0),('scale-p2','scale-s1','two','Same',0),('scale-p3','scale-s2','other','Other',0);
 INSERT INTO tags(id,name) VALUES('scale-common','Common'),('scale-sparse','Sparse'),('scale-folder-sparse','Folder sparse');
 WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<99999)
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,display_name,created_at,updated_at)
 SELECT printf('scale-%06d',i),CASE WHEN i%2=0 THEN 'scale-l1' ELSE 'scale-l2' END,CASE WHEN i%2=0 THEN 'scale-s1' ELSE 'scale-s2' END,CASE WHEN i%4=0 THEN 'scale-p1' WHEN i%4=2 THEN 'scale-p2' ELSE 'scale-p3' END,printf('Name %02d',i%11),CASE WHEN i%3=0 THEN printf('Override %02d',i%7) END,i%11,0 FROM n;
 INSERT INTO album_tags(album_id,tag_id) SELECT id,'scale-common' FROM albums WHERE library_id='scale-l1';
 INSERT INTO album_tags(album_id,tag_id) SELECT id,'scale-sparse' FROM albums WHERE CAST(substr(id,7) AS INTEGER)%997=0;
 INSERT INTO album_tags(album_id,tag_id) SELECT id,'scale-folder-sparse' FROM albums WHERE CAST(substr(id,7) AS INTEGER)%100=0;
 INSERT INTO permission_groups(id,name,created_at) VALUES('scale-folder-group','Folder readers',0);
 INSERT INTO library_access(library_id,group_id) VALUES('scale-l1','scale-folder-group'),('scale-l2','scale-folder-group')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO user_groups(user_id,group_id) VALUES(?,'scale-folder-group')`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	// Real indexed votes drive the native mean, rather than a fake score projection.
	if _, err := f.db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at)
 SELECT ?,id,1+CAST(substr(id,7) AS INTEGER)%5,0 FROM albums WHERE id LIKE 'scale-%' AND CAST(substr(id,7) AS INTEGER)%7!=0`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	// Retain the original sparse favorite and clear distribution for the owner.
	if _, err := f.db.Exec(`INSERT INTO favorites(user_id,album_id,created_at,favorited,updated_at,version)
 SELECT ?,id,0,CASE WHEN CAST(substr(id,7) AS INTEGER)%997=0 OR CAST(substr(id,7) AS INTEGER)%97=0 THEN 1 ELSE 0 END,0,1
 FROM albums WHERE id LIKE 'scale-%' AND (CAST(substr(id,7) AS INTEGER)%997=0 OR CAST(substr(id,7) AS INTEGER)%97=0 OR CAST(substr(id,7) AS INTEGER)%10=0)`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorite_collections(id,user_id,name,parent_id,created_at,updated_at) VALUES('scale-folder',?,'Folder',NULL,0,0),('scale-child',?,'Child','scale-folder',0,0)`, f.other.User.ID, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	// The second real actor has 4% live favorites, a 1% tag and more clear
	// tombstones. A child assignment is never part of the direct parent page.
	if _, err := f.db.Exec(`INSERT INTO favorites(user_id,album_id,collection_id,created_at,favorited,updated_at,version)
 SELECT ?,id,CASE WHEN CAST(substr(id,7) AS INTEGER)%25=0 AND CAST(substr(id,7) AS INTEGER)%8<4 THEN 'scale-folder' WHEN CAST(substr(id,7) AS INTEGER)%25=0 AND CAST(substr(id,7) AS INTEGER)%8=7 THEN 'scale-child' END,0,CASE WHEN CAST(substr(id,7) AS INTEGER)%25=0 THEN 1 ELSE 0 END,0,1
 FROM albums WHERE id LIKE 'scale-%' AND (CAST(substr(id,7) AS INTEGER)%25=0 OR CAST(substr(id,7) AS INTEGER)%10=0)`, f.other.User.ID); err != nil {
		t.Fatal(err)
	}
	checked := [3]int{}
	for _, filter := range []access.AlbumFilter{
		{}, {LibraryID: "scale-l1"}, {SourceID: "scale-s1"}, {PersonID: "scale-p1"},
		{TagID: "scale-common"}, {TagID: "scale-sparse"},
		{LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1"},
		{LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1", TagID: "scale-sparse"},
		{Favorite: "true"}, {Favorite: "true", LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1"},
		{Favorite: "true", TagID: "scale-sparse"},
		{Favorite: "true", LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1", TagID: "scale-sparse"},
		{Favorite: "false"}, {Favorite: "false", LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1", TagID: "scale-common"},
		{FavoriteCollectionID: "scale-folder"},
		{Favorite: "true", FavoriteCollectionID: "scale-folder", LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1"},
		{FavoriteCollectionID: "scale-folder", TagID: "scale-folder-sparse"},
		{FavoriteCollectionID: "scale-folder", LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1", TagID: "scale-common"},
		{FavoriteUnfiled: true},
		{Favorite: "true", FavoriteUnfiled: true, LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1"},
		{FavoriteUnfiled: true, TagID: "scale-folder-sparse"},
		{FavoriteUnfiled: true, LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1", TagID: "scale-common"},
	} {
		for _, order := range []string{"", "latest", "oldest", "name_asc", "name_desc", "top_rated"} {
			filter.Sort = order
			viewer, group := f.owner, 0
			if filter.FavoriteCollectionID != "" {
				viewer, group = f.other, 1
			} else if filter.FavoriteUnfiled {
				viewer, group = f.other, 2
			}
			after, indexedKey := access.AlbumPosition{ID: "scale-050000"}, int64(0)
			var sortKey any = int64(0)
			nameKey, ratingKey := "", 0.0
			switch order {
			case "latest", "oldest":
				after.Key, indexedKey, sortKey = "5", 5, int64(5)
			case "name_asc", "name_desc":
				nameKey = "Name 05"
				if order == "name_desc" {
					nameKey = "Override 02"
				}
				after.Key, sortKey = access.AlbumNameKey(nameKey), nameKey
			case "top_rated":
				ratingKey = 5
				after.Key, sortKey = access.AlbumRatingKey(&ratingKey), ratingKey
			}
			query, arguments := access.AlbumSelectionQuery(viewer.User.ID, filter, after, sortKey, 11)
			rows, err := f.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query, arguments...)
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
			wantIndex := "sqlite_autoindex_albums_1"
			switch {
			case filter.TagID != "":
				wantIndex = "idx_album_tag_reverse"
			case group != 0:
				wantIndex = "idx_favorites_live_collection_page"
			case filter.Favorite == "true":
				wantIndex = "idx_favorites_active_page"
			case filter.PersonID != "":
				wantIndex = "idx_album_person"
			case filter.SourceID != "":
				wantIndex = "idx_album_source_page"
			case filter.LibraryID != "":
				wantIndex = "idx_albums_library"
			}
			if order != "" {
				wantIndex = "idx_album_indexed_page"
				switch {
				case filter.PersonID != "":
					wantIndex = "idx_album_person_indexed_page"
				case filter.SourceID != "":
					wantIndex = "idx_album_source_indexed_page"
				case filter.LibraryID != "":
					wantIndex = "idx_album_library_indexed_page"
				}
				if strings.HasPrefix(order, "name_") {
					wantIndex = strings.Replace(wantIndex, "indexed", "title", 1)
				} else if order == "top_rated" {
					wantIndex = strings.Replace(wantIndex, "indexed", "rating", 1)
				}
			}
			if err != nil || strings.Contains(plan.String(), "TEMP B-TREE") || !strings.Contains(plan.String(), wantIndex) || strings.Contains(query, "OFFSET") {
				t.Fatal("album selection lost selective ordered keysets", filter, plan.String(), err)
			}
			if group != 0 && order == "" && filter.TagID == "" {
				indexed := false
				for _, line := range strings.Split(plan.String(), "\n") {
					indexed = indexed || strings.Contains(line, "SEARCH selection ") && strings.Contains(line, "idx_favorites_live_collection_page") && strings.Contains(line, "user_id=?") && strings.Contains(line, "collection_id=?") && strings.Contains(line, "album_id>?")
				}
				if !indexed {
					t.Fatal("folder ID page lost its account/collection/album range seek", filter, plan.String())
				}
			}
			if (filter.Favorite != "" || group != 0) && (order != "" || filter.TagID != "" || filter.Favorite == "false") {
				indexed := false
				for _, line := range strings.Split(plan.String(), "\n") {
					indexed = indexed || strings.Contains(line, "SEARCH f ") && strings.Contains(line, "INDEX ") && strings.Contains(line, "user_id=?") && strings.Contains(line, "album_id=?")
				}
				if !indexed {
					t.Fatal("favorite existence check lost its account/album point index", filter, plan.String())
				}
			}
			items, err := f.service.Albums(t.Context(), viewer, filter, after, 11)
			if err != nil || len(items) != 11 {
				t.Fatal("large selection did not return only its bounded cards", filter, len(items), err)
			}
			previousID, previousKey, previousTitle, previousRating := after.ID, indexedKey, nameKey, ratingKey
			for _, item := range items {
				number, err := strconv.Atoi(strings.TrimPrefix(item.ID, "scale-"))
				if err != nil {
					t.Fatal("large selection included a non-fixture album", item.ID, err)
				}
				if group != 0 {
					if number%25 != 0 || group == 1 && number%8 >= 4 || group == 2 && (number%8 < 4 || number%8 == 7) || filter.LibraryID != "" && number%2 != 0 || filter.SourceID != "" && number%2 != 0 || filter.PersonID != "" && number%4 != 0 || filter.TagID == "scale-folder-sparse" && number%100 != 0 || filter.TagID == "scale-common" && number%2 != 0 {
						t.Fatal("large folder selection crossed its actor, assignment or other selectors", filter, item.ID)
					}
				} else if filter.Favorite != "" {
					live := number%997 == 0 || number%97 == 0
					if live != (filter.Favorite == "true") {
						t.Fatal("large selection included the wrong live/cleared/absent favorite state", filter, item.ID)
					}
				}
				key, score := item.IndexedAt.UnixMilli(), 0.0
				if item.RatingAvg != nil {
					score = *item.RatingAvg
				}
				ordered := item.ID > previousID
				if order == "oldest" {
					ordered = key > previousKey || key == previousKey && item.ID > previousID
				} else if order == "latest" {
					ordered = key < previousKey || key == previousKey && item.ID < previousID
				} else if order == "name_asc" {
					ordered = item.Title > previousTitle || item.Title == previousTitle && item.ID > previousID
				} else if order == "name_desc" {
					ordered = item.Title < previousTitle || item.Title == previousTitle && item.ID < previousID
				} else if order == "top_rated" {
					ordered = score < previousRating || score == previousRating && item.ID < previousID
				}
				if !ordered || item.MediaCount != 0 || item.CoverMediaID != nil {
					t.Fatal("large selection repeated keys or invented count/cover", filter, item)
				}
				previousID, previousKey, previousTitle, previousRating = item.ID, key, item.Title, score
			}
			checked[group]++
		}
	}
	if checked != [3]int{84, 24, 24} {
		t.Fatal("original and folder query-plan/result matrices were not fully checked", checked)
	}
	foreign, err := f.service.Albums(t.Context(), f.owner, access.AlbumFilter{FavoriteCollectionID: "scale-folder"}, access.AlbumPosition{}, 11)
	if err != nil || foreign == nil || len(foreign) != 0 {
		t.Fatal("another account's large folder leaked through its ID", foreign, err)
	}
}
