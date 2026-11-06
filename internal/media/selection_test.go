package media

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/auth"
)

func mediaPositionForTest(item Metadata, filter AlbumMediaFilter) AlbumMediaPosition {
	position := AlbumMediaPosition{ID: item.ID}
	if strings.HasPrefix(filter.Sort, "captured_") {
		position.Capture = "unknown"
		if item.CaptureAt != nil {
			position.Capture = strconv.FormatInt(item.CaptureAt.UnixMilli(), 10)
		}
	}
	return position
}

func selectionIDs(items []Metadata) []string {
	ids := []string{}
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func seedMediaSelection(t *testing.T, f mediaFixture) time.Time {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE media SET availability='archived'`); err != nil {
		t.Fatal(err)
	}
	captured := time.Date(2026, 11, 6, 1, 2, 3, 123000000, time.UTC)
	for _, row := range []struct {
		id, kind string
		capture  any
	}{
		{"a-unknown", "image", nil},
		{"b-before", "video", time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()},
		{"c-equal", "image", captured.UnixMilli()},
		{"d-equal", "video", captured.UnixMilli()},
		{"e-after", "image", captured.Add(time.Millisecond).UnixMilli()},
		{"f-unknown", "video", nil},
		{"g-later", "video", captured.Add(time.Hour).UnixMilli()},
	} {
		// Legal processing metadata has no source-entry/current-revision yet. Never
		// reuse one indexed entry for multiple media or manufacture a READY row.
		if _, err := f.db.Exec(`INSERT INTO media(id,album_id,kind,file_name,capture_at,created_at,updated_at) VALUES(?,'album',?,'Same label.png',?,0,0)`, row.id, row.kind, row.capture); err != nil {
			t.Fatal(err)
		}
	}
	return captured
}

func TestAlbumMediaSelectionFiltersNullsEqualTimestampsAndBothKeyDirections(t *testing.T) {
	f := fixture(t)
	captured := seedMediaSelection(t, f)
	before := captured.Add(time.Millisecond)
	// Missing source storage must not cause metadata selection to touch/create it.
	if err := os.Rename(f.source, f.source+"-offline"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		filter AlbumMediaFilter
		ids    []string
	}{
		{AlbumMediaFilter{}, []string{"a-unknown", "b-before", "c-equal", "d-equal", "e-after", "f-unknown", "g-later"}},
		{AlbumMediaFilter{Sort: "id_desc"}, []string{"g-later", "f-unknown", "e-after", "d-equal", "c-equal", "b-before", "a-unknown"}},
		{AlbumMediaFilter{Sort: "captured_asc"}, []string{"a-unknown", "f-unknown", "b-before", "c-equal", "d-equal", "e-after", "g-later"}},
		{AlbumMediaFilter{Sort: "captured_desc"}, []string{"g-later", "e-after", "d-equal", "c-equal", "b-before", "f-unknown", "a-unknown"}},
		{AlbumMediaFilter{Kind: "image", Sort: "captured_desc"}, []string{"e-after", "c-equal", "a-unknown"}},
		{AlbumMediaFilter{Kind: "video", Sort: "captured_asc"}, []string{"f-unknown", "b-before", "d-equal", "g-later"}},
		{AlbumMediaFilter{CapturedFrom: &captured, CapturedBefore: &before}, []string{"c-equal", "d-equal"}},
		{AlbumMediaFilter{Kind: "video", CapturedFrom: &captured, CapturedBefore: &before, Sort: "captured_desc"}, []string{"d-equal"}},
		{AlbumMediaFilter{CapturedBefore: &before, Sort: "captured_asc"}, []string{"b-before", "c-equal", "d-equal"}},
	} {
		items, err := f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, test.filter, AlbumMediaPosition{}, 101)
		if err != nil || !slices.Equal(selectionIDs(items), test.ids) {
			t.Fatal("SQL selection/order differs", test.filter, selectionIDs(items), test.ids, err)
		}
		var position AlbumMediaPosition
		seen := []string{}
		for range len(test.ids) + 1 {
			page, err := f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, test.filter, position, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			seen = append(seen, page[0].ID)
			position = mediaPositionForTest(page[0], test.filter)
		}
		if !slices.Equal(seen, test.ids) {
			t.Fatal("bounded pages lost/repeated null or tied keys", test.filter, seen, test.ids)
		}
	}
	filter := AlbumMediaFilter{Sort: "captured_desc"}
	items, err := f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, filter, AlbumMediaPosition{}, 1)
	if err != nil || len(items) != 1 {
		t.Fatal(items, err)
	}
	position := mediaPositionForTest(items[0], filter)
	if _, err := f.db.Exec(`DELETE FROM media WHERE id=?`, position.ID); err != nil {
		t.Fatal(err)
	}
	items, err = f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, filter, position, 1)
	if err != nil || len(items) != 1 || items[0].ID != "e-after" {
		t.Fatal("deleted anchor retargeted the captured keyset", items, err)
	}
	original := filepath.Join(f.source+"-offline", "images", "fixture.png")
	if data, err := os.ReadFile(original); err != nil || !bytes.Equal(data, f.data) {
		t.Fatal("metadata selection changed source bytes", err)
	}
	if _, err := os.Stat(f.source); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("metadata reader recreated a missing source", err)
	}
}

func TestAlbumMediaSelectionRejectsInvalidFiltersAndPositions(t *testing.T) {
	valid := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	fraction := valid.Add(time.Nanosecond)
	tooEarly := time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, filter := range []AlbumMediaFilter{
		{Kind: "Image"}, {Kind: "image' OR 1=1"}, {Kind: "animated"}, {Sort: "name_asc"},
		{Sort: "id_desc; SELECT 1"}, {CapturedFrom: &fraction}, {CapturedBefore: &tooEarly},
		{CapturedFrom: &valid, CapturedBefore: &valid}, {CapturedFrom: &fraction, CapturedBefore: &valid},
	} {
		if !errors.Is(filter.Validate(), ErrMediaSelection) {
			t.Fatal("unsupported selection accepted", filter)
		}
	}
	for _, test := range []struct {
		filter   AlbumMediaFilter
		position AlbumMediaPosition
	}{
		{AlbumMediaFilter{}, AlbumMediaPosition{Capture: "unknown"}},
		{AlbumMediaFilter{}, AlbumMediaPosition{ID: "media", Capture: "0"}},
		{AlbumMediaFilter{}, AlbumMediaPosition{ID: strings.Repeat("x", 65)}},
		{AlbumMediaFilter{}, AlbumMediaPosition{ID: string([]byte{0xff})}},
		{AlbumMediaFilter{Sort: "captured_asc"}, AlbumMediaPosition{ID: "media"}},
		{AlbumMediaFilter{Sort: "captured_desc"}, AlbumMediaPosition{ID: "media", Capture: "01"}},
		{AlbumMediaFilter{Sort: "captured_desc"}, AlbumMediaPosition{ID: "media", Capture: "+1"}},
		{AlbumMediaFilter{Sort: "captured_asc"}, AlbumMediaPosition{ID: "media", Capture: "1.1"}},
		{AlbumMediaFilter{Sort: "captured_asc"}, AlbumMediaPosition{ID: "media", Capture: strings.Repeat("1", 33)}},
		{AlbumMediaFilter{Sort: "captured_asc"}, AlbumMediaPosition{ID: "media", Capture: strconv.FormatInt(math.MaxInt64, 10)}},
		{AlbumMediaFilter{Sort: "captured_asc"}, AlbumMediaPosition{ID: "media", Capture: strconv.FormatInt(math.MinInt64, 10)}},
	} {
		if _, err := test.position.captureKey(test.filter); !errors.Is(err, ErrMediaPosition) {
			t.Fatal("unsupported cursor key accepted", test, err)
		}
	}
	if key, err := (AlbumMediaPosition{ID: "media", Capture: "unknown"}).captureKey(AlbumMediaFilter{Sort: "captured_desc"}); err != nil || key != math.MinInt64 {
		t.Fatal("unknown capture was converted into a date", key, err)
	}
}

func TestAlbumMediaSelectionUsesFreshVisibilityBeforeReturningAnEmptyFilter(t *testing.T) {
	f := fixture(t)
	filter := AlbumMediaFilter{Kind: "video", Sort: "captured_desc"}
	for _, change := range []struct{ apply, restore string }{
		{`UPDATE albums SET access_restricted=1`, `UPDATE albums SET access_restricted=0`},
		{`DELETE FROM library_access`, `INSERT INTO library_access(library_id,group_id) VALUES('library','group')`},
		{`UPDATE libraries SET is_active=0`, `UPDATE libraries SET is_active=1`},
		{`UPDATE sources SET is_active=0`, `UPDATE sources SET is_active=1`},
	} {
		if _, err := f.db.Exec(change.apply); err != nil {
			t.Fatal(err)
		}
		forged := f.reader
		forged.User.Role = "admin"
		if _, err := f.svc.AlbumMedia(t.Context(), forged, f.albumID, filter, AlbumMediaPosition{}, 10); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("hidden album was revealed by an empty selection/forged role", err)
		}
		if _, err := f.db.Exec(change.restore); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.AlbumMedia(t.Context(), f.reader, "missing", filter, AlbumMediaPosition{}, 10); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("missing album leaked an empty filter result", err)
	}
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.reader.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, filter, AlbumMediaPosition{}, 10); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("selection trusted a disabled session DTO", err)
	}
}

func TestAlbumMediaSelection100KUsesOrderedIndexesAndBoundedResults(t *testing.T) {
	f := fixture(t)
	if _, err := f.db.Exec(`UPDATE media SET availability='archived'`); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := f.db.Exec(`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<99999)
 INSERT INTO media(id,album_id,kind,file_name,capture_at,created_at,updated_at)
 SELECT printf('scale-%06d',i),'album',CASE WHEN i%2=0 THEN 'image' ELSE 'video' END,'Processing.png',CASE WHEN i%5=0 THEN NULL ELSE ?+(i/2)%100 END,0,0 FROM n`, base.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	from, before := base.Add(25*time.Millisecond), base.Add(75*time.Millisecond)
	for _, kind := range []string{"", "image", "video"} {
		for _, order := range []string{"id_asc", "id_desc", "captured_asc", "captured_desc"} {
			filter := AlbumMediaFilter{Kind: kind, Sort: order, CapturedFrom: &from, CapturedBefore: &before}
			position := AlbumMediaPosition{ID: "scale-050000"}
			if strings.HasPrefix(order, "captured_") {
				position.Capture = strconv.FormatInt(base.Add(50*time.Millisecond).UnixMilli(), 10)
			}
			key, err := position.captureKey(filter)
			if err != nil {
				t.Fatal(err)
			}
			query, arguments := albumMediaSelection(f.reader.User.ID, f.albumID, filter, position, key, 51)
			rows, err := f.db.Query(`EXPLAIN QUERY PLAN `+query, arguments...)
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
			if err != nil || strings.Contains(plan.String(), "TEMP B-TREE") || !strings.Contains(plan.String(), "SEARCH m USING INDEX idx_media_album") || strings.Contains(query, "OFFSET") {
				t.Fatal("selection lost ordered indexed keyset pagination", filter, plan.String(), err)
			}
			items, err := f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, filter, position, 51)
			if err != nil || len(items) != 51 {
				t.Fatal("100k selection did not return only its bounded page", filter, len(items), err)
			}
			for _, item := range items {
				if item.CaptureAt == nil || item.CaptureAt.Before(from) || !item.CaptureAt.Before(before) || (kind != "" && item.Kind != kind) || item.Revision != nil || item.OfflineCapable {
					t.Fatal("100k predicates or processing authority were fabricated", item)
				}
			}
		}
	}
}
