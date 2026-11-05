package media

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"gallerygo/internal/auth"
)

// These isolated indexed rows intentionally require no source read: the API
// classifies committed fingerprints, not current filesystem/decoder evidence.
func seedDuplicate(t *testing.T, tx *sql.Tx, id, revision, album, source, hash string, size int64) {
	t.Helper()
	queries := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,sha256,source_config_version,updated_at) VALUES(?,?,?, ?,0,?,1,0)`, []any{"entry-" + id, source, "duplicates/" + id + ".png", max(0, size), hash}},
		{`INSERT INTO media(id,album_id,source_entry_id,kind,file_name,mime,width,height,current_revision_id,created_at,updated_at) VALUES(?,?,?,'image',?,'image/png',8,8,?,0,0)`, []any{id, album, "entry-" + id, id + ".png", revision}},
		{`INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) VALUES(?,?,?,?,0,1,0)`, []any{revision, id, hash, size}},
	}
	for _, query := range queries {
		if _, err := tx.Exec(query.query, query.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestExactDuplicatesUseOnlyCurrentOwnedSafeIndexedSignatures(t *testing.T) {
	f := fixture(t)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seedDuplicate(t, tx, "copy-a", "a-revision", "album", "source", f.hash, int64(len(f.data)))
	seedDuplicate(t, tx, "copy-b", "b-revision", "album", "source", f.hash, int64(len(f.data)))
	seedDuplicate(t, tx, "wrong-size", "c-revision", "album", "source", f.hash, int64(len(f.data)+1))
	seedDuplicate(t, tx, "other-a", "d-revision", "album", "source", strings.Repeat("d", 64), 100)
	seedDuplicate(t, tx, "other-b", "e-revision", "album", "source", strings.Repeat("d", 64), 100)
	for index, hash := range []string{strings.ToUpper(f.hash), "invalid", strings.Repeat("g", 64), f.hash + "\x00/private-tail"} {
		for copy := range 2 {
			id := fmt.Sprintf("invalid-%d-%d", index, copy)
			seedDuplicate(t, tx, id, "revision-"+id, "album", "source", hash, 100)
		}
	}
	for copy := range 2 {
		id := fmt.Sprintf("invalid-size-%d", copy)
		seedDuplicate(t, tx, id, "revision-"+id, "album", "source", strings.Repeat("9", 64), 100)
		if _, err := tx.Exec(`UPDATE media_revisions SET source_size='invalid-private-size' WHERE media_id=?`, id); err != nil {
			t.Fatal(err)
		}
		id = fmt.Sprintf("invalid-blob-%d", copy)
		seedDuplicate(t, tx, id, "revision-"+id, "album", "source", strings.Repeat("8", 64), 100)
		if _, err := tx.Exec(`UPDATE media_revisions SET source_sha256=CAST(source_sha256 AS BLOB) WHERE media_id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	seedDuplicate(t, tx, "zero-a", "zero-revision-a", "album", "source", strings.Repeat("0", 64), 0)
	seedDuplicate(t, tx, "zero-b", "zero-revision-b", "album", "source", strings.Repeat("0", 64), 0)
	if _, err := tx.Exec(`INSERT INTO media_revisions VALUES('historical-only','media',?,100,0,2,0)`, strings.Repeat("d", 64)); err != nil {
		t.Fatal(err)
	}
	seedDuplicate(t, tx, "foreign-owner", "foreign-revision", "album", "source", f.hash, int64(len(f.data)))
	if _, err := tx.Exec(`UPDATE media SET current_revision_id='a-revision' WHERE id='foreign-owner'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	groups, err := f.svc.ExactDuplicates(t.Context(), f.owner, DuplicateFilter{}, "", 30)
	if err != nil || len(groups) != 2 || groups[0] != (ExactDuplicateGroup{"a-revision", int64(len(f.data)), 3}) || groups[1] != (ExactDuplicateGroup{"d-revision", 100, 2}) {
		t.Fatal("indexed grouping mixed stale/invalid/size/ownership evidence", groups, err)
	}
	members, err := f.svc.ExactDuplicateMedia(t.Context(), f.owner, groups[0].ID, DuplicateFilter{}, "", 30)
	if err != nil || len(members) != 3 || members[0].ID != "copy-a" || members[1].ID != "copy-b" || members[2].ID != "media" {
		t.Fatal("exact members are not current owned media", members, err)
	}
	encoded, err := json.Marshal(struct {
		Groups  []ExactDuplicateGroup `json:"groups"`
		Members []Metadata            `json:"members"`
	}{groups, members})
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{f.hash, strings.Repeat("d", 64), f.source, "root_path", "source_sha256", "relative_path", "marker"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("duplicate metadata exposed private fingerprint/path", private, string(encoded))
		}
	}
	if err := os.Rename(f.source, f.source+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	unmounted, err := f.svc.ExactDuplicates(t.Context(), f.owner, DuplicateFilter{}, "", 30)
	if err != nil || len(unmounted) != 2 || unmounted[0] != groups[0] {
		t.Fatal("indexed read unexpectedly probed/deleted an unavailable source", unmounted, err)
	}
}

func TestExactDuplicateAnchorRewritesCannotRetargetMemberPages(t *testing.T) {
	f := fixture(t)
	tx, _ := f.db.BeginTx(t.Context(), nil)
	seedDuplicate(t, tx, "copy-a", "a-revision", "album", "source", f.hash, int64(len(f.data)))
	seedDuplicate(t, tx, "copy-b", "b-revision", "album", "source", f.hash, int64(len(f.data)))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	members, err := f.svc.ExactDuplicateMedia(t.Context(), f.owner, "a-revision", DuplicateFilter{}, "copy-a", 1)
	if err != nil || len(members) != 1 || members[0].ID != "copy-b" {
		t.Fatal("member keyset was not by stable media ID", members, err)
	}
	if _, err := f.svc.ExactDuplicateMedia(t.Context(), f.owner, "b-revision", DuplicateFilter{}, "", 30); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("nonrepresentative revision became an alias", err)
	}
	if _, err := f.db.Exec(`INSERT INTO media_revisions VALUES('z-new-revision','copy-a',?,99,0,2,0); UPDATE media SET current_revision_id='z-new-revision',version=version+1 WHERE id='copy-a'`, strings.Repeat("f", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ExactDuplicateMedia(t.Context(), f.owner, "a-revision", DuplicateFilter{}, "copy-a", 30); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("old anchor cursor was retargeted after overwrite", err)
	}
	groups, err := f.svc.ExactDuplicates(t.Context(), f.owner, DuplicateFilter{}, "", 30)
	if err != nil || len(groups) != 1 || groups[0].ID != "b-revision" || groups[0].MediaCount != 2 {
		t.Fatal("rewritten signature polluted the retained duplicate group", groups, err)
	}
	if _, err := f.db.Exec(`UPDATE media SET availability='archived' WHERE id='copy-b'`); err != nil {
		t.Fatal(err)
	}
	groups, err = f.svc.ExactDuplicates(t.Context(), f.owner, DuplicateFilter{}, "", 30)
	if err != nil || groups == nil || len(groups) != 0 {
		t.Fatal("singleton/archived media remained a duplicate group", groups, err)
	}
}

func TestExactDuplicateFiltersApplyBeforeCountsAndReuseSharedVisibility(t *testing.T) {
	f := fixture(t)
	tx, _ := f.db.BeginTx(t.Context(), nil)
	if _, err := tx.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('other-library','Other',0);
 INSERT INTO sources(id,library_id,name,root_path,mapping_json,state,created_at) VALUES('other-source','other-library','other','/never-open-this-source','{}','unavailable',0);
 INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES('other-album','other-library','other-source','Other',0,0)`); err != nil {
		t.Fatal(err)
	}
	seedDuplicate(t, tx, "copy-a", "a-revision", "album", "source", f.hash, int64(len(f.data)))
	seedDuplicate(t, tx, "copy-b", "b-revision", "other-album", "other-source", f.hash, int64(len(f.data)))
	seedDuplicate(t, tx, "copy-c", "c-revision", "other-album", "other-source", f.hash, int64(len(f.data)))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		filter DuplicateFilter
		id     string
		count  int64
	}{
		{DuplicateFilter{}, "a-revision", 4},
		{DuplicateFilter{SourceID: "source"}, "a-revision", 2},
		{DuplicateFilter{LibraryID: "other-library"}, "b-revision", 2},
		{DuplicateFilter{LibraryID: "other-library", SourceID: "other-source"}, "b-revision", 2},
	} {
		groups, err := f.svc.ExactDuplicates(t.Context(), f.owner, test.filter, "", 30)
		if err != nil || len(groups) != 1 || groups[0].ID != test.id || groups[0].MediaCount != test.count {
			t.Fatal("filtered counts used an unfiltered corpus", test, groups, err)
		}
		members, err := f.svc.ExactDuplicateMedia(t.Context(), f.owner, test.id, test.filter, "", 30)
		if err != nil || int64(len(members)) != test.count {
			t.Fatal("member selection differs from filtered counts", test, members, err)
		}
	}
	if _, err := f.svc.ExactDuplicateMedia(t.Context(), f.owner, "a-revision", DuplicateFilter{SourceID: "other-source"}, "", 30); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("filter changed without changing the representative", err)
	}
	for _, statement := range []string{
		`UPDATE libraries SET is_active=0 WHERE id='other-library'`,
		`UPDATE libraries SET is_active=1; UPDATE sources SET is_active=0 WHERE id='other-source'`,
		`UPDATE sources SET is_active=1; UPDATE sources SET library_id='library' WHERE id='other-source'`,
		`UPDATE sources SET library_id='other-library' WHERE id='other-source'; UPDATE source_entries SET source_id='source' WHERE id IN ('entry-copy-b','entry-copy-c')`,
	} {
		if _, err := f.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
		groups, err := f.svc.ExactDuplicates(t.Context(), f.owner, DuplicateFilter{}, "", 30)
		if err != nil || len(groups) != 1 || groups[0].MediaCount != 2 {
			t.Fatal("duplicate query bypassed shared source/library/entry visibility", groups, err)
		}
	}
}

func TestExactDuplicatesUseBoundedStableGroupAndMemberPages(t *testing.T) {
	f := fixture(t)
	tx, _ := f.db.BeginTx(t.Context(), nil)
	for index := range 105 {
		for copy := range 2 {
			id := fmt.Sprintf("group-%03d-%d", index, copy)
			seedDuplicate(t, tx, id, "revision-"+id, "album", "source", fmt.Sprintf("%064x", index+1), 123)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	first, err := f.svc.ExactDuplicates(t.Context(), f.owner, DuplicateFilter{}, "", 100)
	if err != nil || len(first) != 100 {
		t.Fatal("bounded group page failed", len(first), err)
	}
	last, err := f.svc.ExactDuplicates(t.Context(), f.owner, DuplicateFilter{}, first[99].ID, 100)
	if err != nil || len(last) != 5 || last[0].ID != "revision-group-100-0" || last[4].MediaCount != 2 {
		t.Fatal("post-group cursor lost counts/rows", last, err)
	}
	for _, limit := range []int{0, 102} {
		if _, err := f.svc.ExactDuplicates(t.Context(), f.owner, DuplicateFilter{}, "", limit); !errors.Is(err, ErrDuplicateQuery) {
			t.Fatal("invalid group limit accepted", err)
		}
		if _, err := f.svc.ExactDuplicateMedia(t.Context(), f.owner, first[0].ID, DuplicateFilter{}, "", limit); !errors.Is(err, ErrDuplicateQuery) {
			t.Fatal("invalid member limit accepted", err)
		}
	}
	for _, filter := range []DuplicateFilter{{SourceID: strings.Repeat("s", 65)}, {LibraryID: "../private"}, {SourceID: "bad\x00"}, {LibraryID: " padded "}} {
		if _, err := f.svc.ExactDuplicates(t.Context(), f.owner, filter, "", 30); !errors.Is(err, ErrDuplicateQuery) {
			t.Fatal("invalid duplicate filter accepted", filter, err)
		}
	}
}

func TestExactDuplicateReadsRecheckCurrentAccountAndCancelledContext(t *testing.T) {
	f := fixture(t)
	forged := f.reader
	forged.User.Role = "admin"
	for _, call := range []func(context.Context, auth.Session) error{
		func(ctx context.Context, session auth.Session) error {
			_, err := f.svc.ExactDuplicates(ctx, session, DuplicateFilter{}, "", 30)
			return err
		},
		func(ctx context.Context, session auth.Session) error {
			_, err := f.svc.ExactDuplicateMedia(ctx, session, "missing", DuplicateFilter{}, "", 30)
			return err
		},
	} {
		if err := call(t.Context(), forged); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("caller DTO role bypassed current SQL authorization", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := call(ctx, f.owner); !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled duplicate query ran", err)
		}
	}
	if _, err := f.db.Exec(`UPDATE users SET role='user',version=version+1 WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ExactDuplicates(t.Context(), f.owner, DuplicateFilter{}, "", 30); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("stale administrator role authorized a duplicate read", err)
	}
}

func FuzzDuplicateFiltersRejectUnboundedOrUnsafeIdentifiers(f *testing.F) {
	for _, value := range []string{"", "source", "來源", "../private", "bad\x00", string(rune(0x202e)) + "private", strings.Repeat("x", 65), string([]byte{0xff})} {
		f.Add(value, value)
	}
	f.Fuzz(func(t *testing.T, library, source string) {
		filter := DuplicateFilter{LibraryID: library, SourceID: source}
		if err := filter.Validate(); err != nil {
			if !errors.Is(err, ErrDuplicateQuery) {
				t.Fatal("filter returned a non-contract diagnostic", err)
			}
			return
		}
		for _, value := range []string{library, source} {
			if len(value) > 64 || strings.ContainsAny(value, "/\\\x00\r\n") || strings.TrimSpace(value) != value {
				t.Fatal("unsafe identifier accepted", filter)
			}
		}
	})
}
