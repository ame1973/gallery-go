package access

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gallerygo/internal/auth"
)

func albumSelectionFixture(t *testing.T) personFixture {
	t.Helper()
	f := indexedPeopleFixture(t)
	if _, err := f.svc.db.Exec(`INSERT INTO tags(id,name) VALUES('selection-common','Private shared label'),('selection-other','Other'),('selection-unused','Unused');
 INSERT INTO album_tags(album_id,tag_id) VALUES
 ('person-open','selection-common'),('person-private','selection-common'),('person-other','selection-common'),
 ('person-foreign','selection-common'),('person-no-path-album','selection-common'),('person-other','selection-other')`); err != nil {
		t.Fatal(err)
	}
	return f
}

func albumSelectionIDs(items []Album) []string {
	ids := []string{}
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func TestAlbumSelectionIntersectsLiteralResourcesAndKeepsAuthorizedCardProjection(t *testing.T) {
	f := albumSelectionFixture(t)
	for _, test := range []struct {
		viewer auth.Session
		filter AlbumFilter
		want   []string
	}{
		{f.base, AlbumFilter{PersonID: "person-one"}, []string{"person-open"}},
		{f.both, AlbumFilter{PersonID: "person-one"}, []string{"person-open", "person-private"}},
		{f.base, AlbumFilter{PersonID: "person-two"}, []string{"person-other"}},
		{f.base, AlbumFilter{TagID: "selection-other"}, []string{"person-other"}},
		{f.base, AlbumFilter{TagID: "selection-common"}, []string{"person-no-path-album", "person-open", "person-other"}},
		{f.both, AlbumFilter{LibraryID: "person-library", SourceID: "person-source", PersonID: "person-one", TagID: "selection-common"}, []string{"person-open", "person-private"}},
		{f.owner, AlbumFilter{PersonID: "person-hidden"}, []string{"person-foreign"}},
		{f.base, AlbumFilter{PersonID: "person-hidden"}, []string{}},
		{f.owner, AlbumFilter{SourceID: "person-source", PersonID: "person-hidden"}, []string{}},
		{f.owner, AlbumFilter{LibraryID: "other-person-library", SourceID: "person-source"}, []string{}},
		{f.base, AlbumFilter{LibraryID: "other-person-library"}, []string{}},
		{f.base, AlbumFilter{SourceID: "other-person-source"}, []string{}},
		{f.owner, AlbumFilter{PersonID: "person-no-path"}, []string{}},
		{f.owner, AlbumFilter{PersonID: "person-root-path"}, []string{}},
		{f.owner, AlbumFilter{PersonID: "person-unclassified"}, []string{}},
		{f.base, AlbumFilter{TagID: "selection-unused"}, []string{}},
		{f.owner, AlbumFilter{TagID: "unknown-tag"}, []string{}},
		{f.base, AlbumFilter{SourceID: "unknown-source"}, []string{}},
		{f.owner, AlbumFilter{LibraryID: "unknown-library"}, []string{}},
	} {
		items, err := f.svc.Albums(t.Context(), test.viewer, test.filter, AlbumPosition{}, 101)
		if err != nil || items == nil || !slices.Equal(albumSelectionIDs(items), test.want) {
			t.Fatal("album selectors widened access or ignored their intersection", test.filter, albumSelectionIDs(items), test.want, err)
		}
		for _, item := range items {
			detail, err := f.svc.Album(t.Context(), test.viewer, item.ID)
			if err != nil || !reflect.DeepEqual(item, detail) {
				t.Fatal("selection changed authorized count/cover/card semantics", item, detail, err)
			}
			if item.ID == "person-open" && (item.MediaCount != 1 || item.CoverMediaID != nil) {
				t.Fatal("selection counted archived/foreign-source media or leaked a cross-album cover", item)
			}
		}
	}
	filter := AlbumFilter{SourceID: "person-source", PersonID: "person-one", TagID: "selection-common"}
	var seen []string
	after := ""
	for range 3 {
		items, err := f.svc.Albums(t.Context(), f.both, filter, AlbumPosition{ID: after}, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		seen = append(seen, items[0].ID)
		after = items[0].ID
	}
	if !slices.Equal(seen, []string{"person-open", "person-private"}) {
		t.Fatal("filtered stable-ID pages lost or repeated albums", seen)
	}
	forged := f.base
	forged.User.Role = "admin"
	items, err := f.svc.Albums(t.Context(), forged, AlbumFilter{PersonID: "person-one"}, AlbumPosition{}, 100)
	if err != nil || !slices.Equal(albumSelectionIDs(items), []string{"person-open"}) {
		t.Fatal("selection trusted a forged role instead of the fresh session", items, err)
	}
}

func TestAlbumSelectionFreshPolicySourceAndAccountChangesRemainFailClosed(t *testing.T) {
	f := albumSelectionFixture(t)
	filter := AlbumFilter{TagID: "selection-common", PersonID: "person-one"}
	if err := f.svc.DeleteGroup(t.Context(), f.owner, f.privateGroup.ID, f.privateGroup.Version); err != nil {
		t.Fatal(err)
	}
	items, err := f.svc.Albums(t.Context(), f.both, filter, AlbumPosition{}, 30)
	if err != nil || !slices.Equal(albumSelectionIDs(items), []string{"person-open"}) {
		t.Fatal("removed group reopened restricted albums through selection", items, err)
	}
	var restricted bool
	if err := f.svc.db.QueryRow(`SELECT access_restricted FROM albums WHERE id='person-private'`).Scan(&restricted); err != nil || !restricted {
		t.Fatal("selection/group deletion cleared the restriction latch", err)
	}
	for _, change := range []struct{ apply, restore string }{
		{`UPDATE sources SET is_active=0 WHERE id='person-source'`, `UPDATE sources SET is_active=1 WHERE id='person-source'`},
		{`UPDATE libraries SET is_active=0 WHERE id='person-library'`, `UPDATE libraries SET is_active=1 WHERE id='person-library'`},
		{`UPDATE sources SET library_id='other-person-library' WHERE id='person-source'`, `UPDATE sources SET library_id='person-library' WHERE id='person-source'`},
	} {
		if _, err := f.svc.db.Exec(change.apply); err != nil {
			t.Fatal(err)
		}
		for _, viewer := range []auth.Session{f.owner, f.base, f.both} {
			items, err := f.svc.Albums(t.Context(), viewer, filter, AlbumPosition{}, 30)
			if err != nil || len(items) != 0 {
				t.Fatal("inactive/inconsistent resources escaped a filtered collection", items, err)
			}
		}
		if _, err := f.svc.db.Exec(change.restore); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.base.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Albums(t.Context(), f.base, filter, AlbumPosition{}, 30); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("selection trusted a disabled account DTO", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.svc.Albums(ctx, f.owner, filter, AlbumPosition{}, 30); !errors.Is(err, context.Canceled) {
		t.Fatal("selection ignored cancellation", err)
	}
}

func TestAlbumSelectionRejectsUnboundedPathControlAndAmbiguousIDs(t *testing.T) {
	for _, id := range []string{strings.Repeat("a", 65), "/private/path", `one\two`, " padded", "padded ", "bad\x00", "bad\n", "bad" + string(rune(0x200d)), string([]byte{0xff})} {
		for _, filter := range []AlbumFilter{{LibraryID: id}, {SourceID: id}, {PersonID: id}, {TagID: id}} {
			if !errors.Is(filter.Validate(), ErrAlbumSelection) {
				t.Fatal("invalid opaque resource ID accepted", filter)
			}
		}
	}
	if err := (AlbumFilter{LibraryID: "literal", SourceID: "literal", PersonID: "literal", TagID: "literal"}).Validate(); err != nil {
		t.Fatal("different resource domains incorrectly shared a uniqueness constraint", err)
	}
}

func TestAlbumSelection100KUsesExistingSelectiveIndexesBeforeBoundedCards(t *testing.T) {
	s, owner := fixture(t)
	if _, err := s.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('scale-l1','One',0),('scale-l2','Two',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('scale-s1','scale-l1','One','/never-open-album-scale-one','{}','unavailable',0),('scale-s2','scale-l2','Two','/never-open-album-scale-two','{}','healthy',0);
 INSERT INTO people(id,source_id,relative_path,source_name,created_at) VALUES('scale-p1','scale-s1','one','Same',0),('scale-p2','scale-s1','two','Same',0),('scale-p3','scale-s2','other','Other',0);
 INSERT INTO tags(id,name) VALUES('scale-common','Common'),('scale-sparse','Sparse');
 WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<99999)
 INSERT INTO albums(id,library_id,source_id,person_id,source_name,display_name,created_at,updated_at)
 SELECT printf('scale-%06d',i),CASE WHEN i%2=0 THEN 'scale-l1' ELSE 'scale-l2' END,CASE WHEN i%2=0 THEN 'scale-s1' ELSE 'scale-s2' END,CASE WHEN i%4=0 THEN 'scale-p1' WHEN i%4=2 THEN 'scale-p2' ELSE 'scale-p3' END,printf('Name %02d',i%11),CASE WHEN i%3=0 THEN printf('Override %02d',i%7) END,i%11,0 FROM n;
 INSERT INTO album_tags(album_id,tag_id) SELECT id,'scale-common' FROM albums WHERE library_id='scale-l1';
 INSERT INTO album_tags(album_id,tag_id) SELECT id,'scale-sparse' FROM albums WHERE CAST(substr(id,7) AS INTEGER)%997=0`); err != nil {
		t.Fatal(err)
	}
	// Real indexed votes drive the derived native mean; no fake score update seeds
	// the ranking field. Existing SQL setup stays isolated from production sources.
	if _, err := s.db.Exec(`INSERT INTO ratings(user_id,album_id,rating,updated_at)
 SELECT ?,id,1+CAST(substr(id,7) AS INTEGER)%5,0 FROM albums
 WHERE id LIKE 'scale-%' AND CAST(substr(id,7) AS INTEGER)%7!=0`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, filter := range []AlbumFilter{
		{}, {LibraryID: "scale-l1"}, {SourceID: "scale-s1"}, {PersonID: "scale-p1"},
		{TagID: "scale-common"}, {TagID: "scale-sparse"},
		{LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1"},
		{LibraryID: "scale-l1", SourceID: "scale-s1", PersonID: "scale-p1", TagID: "scale-sparse"},
	} {
		for _, order := range []string{"", "latest", "oldest", "name_asc", "name_desc", "top_rated"} {
			filter.Sort = order
			after, indexedKey := AlbumPosition{ID: "scale-050000"}, int64(0)
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
				after.Key, sortKey = AlbumNameKey(nameKey), nameKey
			case "top_rated":
				ratingKey = 5
				after.Key, sortKey = AlbumRatingKey(&ratingKey), ratingKey
			}
			query, arguments := albumSelection(owner.User.ID, filter, after, sortKey, 11)
			rows, err := s.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query, arguments...)
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
			items, err := s.Albums(t.Context(), owner, filter, after, 11)
			if err != nil || len(items) != 11 {
				t.Fatal("large selection did not return only its bounded cards", filter, len(items), err)
			}
			previousID, previousKey, previousTitle, previousRating := after.ID, indexedKey, nameKey, ratingKey
			for _, item := range items {
				key, score := item.IndexedAt.UnixMilli(), 0.0
				if item.RatingAvg != nil {
					score = *item.RatingAvg
				}
				ordered := item.ID > previousID
				if order == "oldest" {
					ordered = key > previousKey || (key == previousKey && item.ID > previousID)
				} else if order == "latest" {
					ordered = key < previousKey || (key == previousKey && item.ID < previousID)
				} else if order == "name_asc" {
					ordered = item.Title > previousTitle || (item.Title == previousTitle && item.ID > previousID)
				} else if order == "name_desc" {
					ordered = item.Title < previousTitle || (item.Title == previousTitle && item.ID < previousID)
				} else if order == "top_rated" {
					ordered = score < previousRating || (score == previousRating && item.ID < previousID)
				}
				if !ordered || item.MediaCount != 0 || item.CoverMediaID != nil {
					t.Fatal("large selection repeated keys or invented count/cover", filter, item)
				}
				previousID, previousKey, previousTitle, previousRating = item.ID, key, item.Title, score
			}
		}
	}
}
