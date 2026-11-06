package access

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func albumNameFixture(t *testing.T) (personFixture, []Album) {
	t.Helper()
	f := albumSelectionFixture(t)
	if _, err := f.svc.db.Exec(`INSERT INTO tags(id,name) VALUES('name-order-tag','Literal name selection')`); err != nil {
		t.Fatal(err)
	}
	override, cleared := "Override", ""
	vectors := []struct {
		source  string
		display *string
	}{
		{"", nil}, {" ", nil}, {"\t", nil}, {"10", nil}, {"2", nil},
		{"A", nil}, {"a", nil}, {"é", nil}, {"é", nil}, {"中", nil}, {"😀", nil},
		{"same", nil}, {"same", nil}, {"zz-source", &override}, {"Fallback", &cleared},
		{"' OR 1=1 --", nil}, {`<&"\name>`, nil},
		{strings.Repeat("<", MaxAlbumTitleBytes), nil}, {strings.Repeat("\x01", MaxAlbumTitleBytes), nil},
	}
	expected := []Album{}
	for index, vector := range vectors {
		id := fmt.Sprintf("literal-%02d", index)
		if _, err := f.svc.db.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,display_name,created_at,updated_at) VALUES(?,'person-library','person-source','person-one',?,?,0,0)`, id, vector.source, vector.display); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.db.Exec(`INSERT INTO album_tags(album_id,tag_id) VALUES(?,'name-order-tag')`, id); err != nil {
			t.Fatal(err)
		}
		album, err := f.svc.Album(t.Context(), f.both, id)
		title := vector.source
		if vector.display != nil && *vector.display != "" {
			title = *vector.display
		}
		if err != nil || album.Title != title {
			t.Fatal("shared public title is not the literal effective override/source", id, err)
		}
		expected = append(expected, album)
	}
	if _, err := f.svc.db.Exec(`INSERT INTO albums(id,library_id,source_id,person_id,source_name,access_restricted,created_at,updated_at) VALUES('literal-hidden','person-library','person-source','person-one','',1,0,0);
 INSERT INTO album_tags VALUES('literal-hidden','name-order-tag')`); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(expected, func(a, b Album) int {
		if compared := strings.Compare(a.Title, b.Title); compared != 0 {
			return compared
		}
		return strings.Compare(a.ID, b.ID)
	})
	return f, expected
}

func TestAlbumNameOrderIsLiteralBinaryWithFullPublicKeysAndReversedTies(t *testing.T) {
	f, expected := albumNameFixture(t)
	for _, order := range []string{"name_asc", "name_desc"} {
		want := slices.Clone(expected)
		if order == "name_desc" {
			slices.Reverse(want)
		}
		filter := AlbumFilter{LibraryID: "person-library", SourceID: "person-source", PersonID: "person-one", TagID: "name-order-tag", Sort: order}
		all, err := f.svc.Albums(t.Context(), f.both, filter, AlbumPosition{}, 101)
		if err != nil || !reflect.DeepEqual(all, want) {
			t.Fatal("literal name sort normalized, merged, lost or disclosed a title", order, albumSelectionIDs(all), albumSelectionIDs(want), err)
		}
		after := AlbumPosition{}
		seen := []Album{}
		for range len(want) + 1 {
			page, err := f.svc.Albums(t.Context(), f.both, filter, after, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			seen = append(seen, page[0])
			after = AlbumPosition{ID: page[0].ID, Key: AlbumNameKey(page[0].Title)}
			key, err := after.orderKey(filter)
			if err != nil || key != page[0].Title {
				t.Fatal("name cursor did not freeze the entire exact public title", page[0].ID, err)
			}
		}
		if !reflect.DeepEqual(seen, want) {
			t.Fatal("literal title keysets lost/truncated repeated or maximal names", order, albumSelectionIDs(seen), albumSelectionIDs(want))
		}
	}
	// ASCII/numeric/composition distinctions are intentional; never silently use
	// NOCASE, lowercase, NFKC, a locale comparator or natural numeric ordering.
	positions := map[string]int{}
	for index, album := range expected {
		positions[album.Title] = index
	}
	if expected[0].Title != "" || positions["10"] >= positions["2"] || positions["A"] >= positions["a"] || positions["é"] >= positions["é"] {
		t.Fatal("literal numeric/case/composition distinctions were normalized or reordered")
	}
}

func TestAlbumNameOrderDoesNotRereadDeletedOrRenamedAnchors(t *testing.T) {
	f, expected := albumNameFixture(t)
	filter := AlbumFilter{TagID: "name-order-tag", Sort: "name_asc"}
	first, err := f.svc.Albums(t.Context(), f.both, filter, AlbumPosition{}, 1)
	if err != nil || len(first) != 1 || first[0].ID != expected[0].ID {
		t.Fatal(first, err)
	}
	after := AlbumPosition{ID: first[0].ID, Key: AlbumNameKey(first[0].Title)}
	if after.Key != "n:" {
		t.Fatal("empty title is indistinguishable from missing cursor position", after)
	}
	if _, err := f.svc.db.Exec(`DELETE FROM albums WHERE id=?`, after.ID); err != nil {
		t.Fatal(err)
	}
	next, err := f.svc.Albums(t.Context(), f.both, filter, after, 1)
	if err != nil || len(next) != 1 || next[0].ID != expected[1].ID {
		t.Fatal("name continuation required the deleted anchor", next, err)
	}
	after = AlbumPosition{ID: next[0].ID, Key: AlbumNameKey(next[0].Title)}
	changed := "zz-renamed"
	if _, err := f.svc.UpdateAlbumMetadata(t.Context(), f.owner, next[0].ID, next[0].Version, AlbumMetadataPatch{DisplayName: &changed}, "name-anchor-command-0001"); err != nil {
		t.Fatal(err)
	}
	next, err = f.svc.Albums(t.Context(), f.both, filter, after, 1)
	if err != nil || len(next) != 1 || next[0].ID != expected[2].ID {
		t.Fatal("name continuation retargeted itself to a renamed anchor", next, err)
	}
}

func TestAlbumNameOrderRejectsNoncanonicalOrUnboundedKeysAndStoredTitles(t *testing.T) {
	for _, order := range []string{"name_asc", "name_desc"} {
		filter := AlbumFilter{Sort: order}
		for _, key := range []string{"", "title", "0", "n:YQ==", "n:YR", "n:!", "n:YQ\n", "n:_w", AlbumNameKey(strings.Repeat("a", MaxAlbumTitleBytes+1))} {
			if _, err := (AlbumPosition{ID: "album", Key: key}).orderKey(filter); !errors.Is(err, ErrAlbumPosition) {
				t.Fatal("ambiguous or unbounded name key accepted", len(key), err)
			}
		}
		for _, title := range []string{"", " leading ", "'", "\x00", "é", "é", strings.Repeat("&", MaxAlbumTitleBytes)} {
			key, err := (AlbumPosition{ID: "album", Key: AlbumNameKey(title)}).orderKey(filter)
			if err != nil || key != title {
				t.Fatal("literal title key was changed or truncated", len(title), err)
			}
		}
		if _, err := (AlbumPosition{}).orderKey(filter); err != nil {
			t.Fatal("first name page requires an invented anchor", err)
		}
		if _, err := (AlbumPosition{Key: AlbumNameKey("unbound")}).orderKey(filter); !errors.Is(err, ErrAlbumPosition) {
			t.Fatal("name key without an anchor ID accepted", err)
		}
	}
	f, expected := albumNameFixture(t)
	for _, title := range []string{strings.Repeat("x", MaxAlbumTitleBytes+1), string([]byte{0xff})} {
		if _, err := f.svc.db.Exec(`UPDATE albums SET source_name=? WHERE id=?`, title, expected[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Album(t.Context(), f.owner, expected[0].ID); err == nil {
			t.Fatal("invalid stored title reached an ambiguous public JSON/keyset")
		}
	}
	if base64.RawURLEncoding.EncodedLen(MaxAlbumTitleBytes)+2 != len(AlbumNameKey(strings.Repeat("x", MaxAlbumTitleBytes))) {
		t.Fatal("name-key bound differs from its actual wire encoding")
	}
}
