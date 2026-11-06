package access

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/auth"
)

func albumOrderFixture(t *testing.T) personFixture {
	t.Helper()
	f := albumSelectionFixture(t)
	if _, err := f.svc.db.Exec(`UPDATE albums SET created_at=CASE id
 WHEN 'person-other' THEN -1000 WHEN 'person-no-path-album' THEN 1000
 WHEN 'person-foreign' THEN 5000 ELSE 0 END`); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestAlbumOrderUsesPublicFirstIndexedTimeAndReversesStableTies(t *testing.T) {
	f := albumOrderFixture(t)
	for _, viewer := range []auth.Session{f.base, f.both} {
		for _, order := range []string{"oldest", "latest"} {
			filter := AlbumFilter{LibraryID: "person-library", SourceID: "person-source", TagID: "selection-common", Sort: order}
			want := []string{"person-other", "person-open", "person-private", "person-no-path-album"}
			if viewer.User.ID == f.base.User.ID {
				want = slices.Delete(want, 2, 3)
			}
			if order == "latest" {
				slices.Reverse(want)
			}
			items, err := f.svc.Albums(t.Context(), viewer, filter, AlbumPosition{}, 101)
			if err != nil || !slices.Equal(albumSelectionIDs(items), want) {
				t.Fatal("chronological selection lost shared visibility or stable ties", order, items, want, err)
			}
			for _, item := range items {
				detail, err := f.svc.Album(t.Context(), viewer, item.ID)
				if err != nil || !reflect.DeepEqual(item, detail) || item.IndexedAt.Location() != time.UTC {
					t.Fatal("ordered card changed public time/count/cover semantics", item, detail, err)
				}
				var stored int64
				if err := f.svc.db.QueryRow(`SELECT created_at FROM albums WHERE id=?`, item.ID).Scan(&stored); err != nil || item.IndexedAt.UnixMilli() != stored {
					t.Fatal("indexed_at was guessed from capture/mtime/edit state", item, stored, err)
				}
			}
			after := AlbumPosition{}
			seen := []string{}
			for range 6 {
				page, err := f.svc.Albums(t.Context(), viewer, filter, after, 1)
				if err != nil {
					t.Fatal(err)
				}
				if len(page) == 0 {
					break
				}
				seen = append(seen, page[0].ID)
				after = AlbumPosition{ID: page[0].ID, Key: strconv.FormatInt(page[0].IndexedAt.UnixMilli(), 10)}
			}
			if !slices.Equal(seen, want) {
				t.Fatal("chronological tuple pages lost or repeated a tie", order, seen, want)
			}
			filter.PersonID = "person-one"
			person, err := f.svc.Albums(t.Context(), viewer, filter, AlbumPosition{}, 100)
			personWant := []string{"person-open"}
			if viewer.User.ID == f.both.User.ID {
				personWant = append(personWant, "person-private")
				if order == "latest" {
					slices.Reverse(personWant)
				}
			}
			if err != nil || !slices.Equal(albumSelectionIDs(person), personWant) {
				t.Fatal("creator/tag intersection lost its ordered driver", order, person, err)
			}
		}
	}
}

func TestAlbumOrderPositionSurvivesDeletedAndChangedAnchors(t *testing.T) {
	f := albumOrderFixture(t)
	if _, err := f.svc.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES('date-anchor','person-library','person-source','Empty anchor',2000,0);
 INSERT INTO album_tags VALUES('date-anchor','selection-common')`); err != nil {
		t.Fatal(err)
	}
	filter := AlbumFilter{TagID: "selection-common", SourceID: "person-source", Sort: "latest"}
	first, err := f.svc.Albums(t.Context(), f.both, filter, AlbumPosition{}, 1)
	if err != nil || len(first) != 1 || first[0].ID != "date-anchor" {
		t.Fatal(first, err)
	}
	after := AlbumPosition{ID: first[0].ID, Key: strconv.FormatInt(first[0].IndexedAt.UnixMilli(), 10)}
	if _, err := f.svc.db.Exec(`DELETE FROM albums WHERE id='date-anchor'`); err != nil {
		t.Fatal(err)
	}
	next, err := f.svc.Albums(t.Context(), f.both, filter, after, 1)
	if err != nil || len(next) != 1 || next[0].ID != "person-no-path-album" {
		t.Fatal("continuation reread or required the deleted anchor", next, err)
	}
	// Only isolated SQL fixture data changes. Real APIs never edit first-index time;
	// a live-page continuation must nevertheless retain its already returned key.
	after = AlbumPosition{ID: next[0].ID, Key: strconv.FormatInt(next[0].IndexedAt.UnixMilli(), 10)}
	if _, err := f.svc.db.Exec(`UPDATE albums SET created_at=-9999 WHERE id='person-no-path-album'`); err != nil {
		t.Fatal(err)
	}
	next, err = f.svc.Albums(t.Context(), f.both, filter, after, 1)
	if err != nil || len(next) != 1 || next[0].ID != "person-private" {
		t.Fatal("continuation resolved a changed anchor instead of its frozen key", next, err)
	}
}

func TestAlbumOrderRejectsAmbiguousSortPositionsAndInvalidStoredTime(t *testing.T) {
	for _, order := range []string{"id_desc", "NAME_ASC", "popular", "top_rated", "LATEST", " latest", "oldest;DROP TABLE albums"} {
		if !errors.Is((AlbumFilter{Sort: order}).Validate(), ErrAlbumSelection) {
			t.Fatal("unimplemented/ambiguous album sort accepted", order)
		}
	}
	for _, order := range []string{"latest", "oldest"} {
		filter := AlbumFilter{Sort: order}
		for _, key := range []string{"", "unknown", "01", "-0", "+1", "1.0", "1e3", " 1", "253402300800000", "-62135596800001", "9223372036854775807", strings.Repeat("1", 33)} {
			if _, err := (AlbumPosition{ID: "album", Key: key}).orderKey(filter); !errors.Is(err, ErrAlbumPosition) {
				t.Fatal("invalid public indexed-time key accepted", key, err)
			}
		}
		for _, key := range []string{"0", "-1", "-62135596800000", "253402300799999"} {
			if _, err := (AlbumPosition{ID: "album", Key: key}).orderKey(filter); err != nil {
				t.Fatal("valid bounded public indexed-time key rejected", key, err)
			}
		}
		if _, err := (AlbumPosition{}).orderKey(filter); err != nil {
			t.Fatal("first chronological page requires an invented anchor", err)
		}
	}
	for _, position := range []AlbumPosition{{ID: "album", Key: "0"}, {Key: "0"}, {ID: strings.Repeat("a", 65)}, {ID: string([]byte{0xff})}} {
		if _, err := position.orderKey(AlbumFilter{}); !errors.Is(err, ErrAlbumPosition) {
			t.Fatal("ID-only position accepted an unrelated or unbounded key", position, err)
		}
	}
	f := albumOrderFixture(t)
	if _, err := f.svc.db.Exec(`UPDATE albums SET created_at=253402300800000 WHERE id='person-open'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Album(t.Context(), f.owner, "person-open"); err == nil {
		t.Fatal("stored out-of-range time reached a partial public JSON response")
	}
}
