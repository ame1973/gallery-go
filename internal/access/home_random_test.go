package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/auth"
)

func TestRandomAlbumsUsesFreshAuthorityAndSharedCardProjection(t *testing.T) {
	f := albumSelectionFixture(t)
	var before [3]int64
	if err := f.svc.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`).Scan(&before[0], &before[1], &before[2]); err != nil {
		t.Fatal(err)
	}
	for _, viewer := range []auth.Session{f.base, f.both, f.owner} {
		all, err := f.svc.Albums(t.Context(), viewer, AlbumFilter{}, AlbumPosition{}, 101)
		if err != nil {
			t.Fatal(err)
		}
		sample, err := f.svc.RandomAlbums(t.Context(), viewer, MaxRandomAlbums)
		if err != nil || sample == nil || len(sample) != len(all) {
			t.Fatal("sample lost an eligible album or exposed a hidden one", len(sample), len(all), err)
		}
		seen := map[string]bool{}
		for _, album := range sample {
			if seen[album.ID] {
				t.Fatal("sample returned a duplicate", album.ID)
			}
			seen[album.ID] = true
			detail, err := f.svc.Album(t.Context(), viewer, album.ID)
			if err != nil || !reflect.DeepEqual(album, detail) {
				t.Fatal("sample projection differs from authorized detail", album, detail, err)
			}
		}
		for _, album := range all {
			if !seen[album.ID] {
				t.Fatal("sample omitted an album within its limit", album.ID)
			}
		}
	}
	forged := f.base
	forged.User = f.owner.User
	items, err := f.svc.RandomAlbums(t.Context(), forged, MaxRandomAlbums)
	if err != nil || slices.Contains(albumSelectionIDs(items), "person-private") || slices.Contains(albumSelectionIDs(items), "person-foreign") {
		t.Fatal("sample trusted a forged user or role", items, err)
	}
	for _, limit := range []int{-1, 0, MaxRandomAlbums + 1, 1001} {
		if _, err := f.svc.RandomAlbums(t.Context(), f.base, limit); !errors.Is(err, ErrRandomAlbumLimit) {
			t.Fatal("invalid sample limit accepted", limit, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.svc.RandomAlbums(ctx, f.base, 1); !errors.Is(err, context.Canceled) {
		t.Fatal("sample ignored request cancellation", err)
	}
	var after [3]int64
	if err := f.svc.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events)`).Scan(&after[0], &after[1], &after[2]); err != nil || before != after {
		t.Fatal("read-only samples wrote a durable business event", before, after, err)
	}
	if _, err := f.svc.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.base.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RandomAlbums(t.Context(), f.base, 1); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("sample trusted a disabled account DTO", err)
	}
}

func TestRandomAlbumsUsesDeferredSnapshotAndMaterializesOnlyBoundedCards(t *testing.T) {
	f := albumSelectionFixture(t)
	rows, err := f.svc.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+randomAlbumsQuery, f.base.User.ID, MaxRandomAlbums, f.base.User.ID, f.base.User.ID, f.base.User.ID)
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
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if !strings.Contains(plan.String(), "MATERIALIZE sampled") || !strings.Contains(plan.String(), "SEARCH a USING INDEX sqlite_autoindex_albums_1") || strings.Contains(randomAlbumsQuery, "OFFSET") {
		t.Fatal("sample stopped materializing bounded IDs before projecting album cards", plan.String())
	}
	writer, err := f.svc.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	if _, err := writer.ExecContext(t.Context(), `UPDATE sources SET is_active=0 WHERE id='person-source'`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	before, err := f.svc.RandomAlbums(ctx, f.base, MaxRandomAlbums)
	if err != nil || !slices.Contains(albumSelectionIDs(before), "person-open") {
		t.Fatal("sample could not read the last committed snapshot while a writer was held", before, err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.RandomAlbums(t.Context(), f.base, MaxRandomAlbums)
	if err != nil || after == nil || len(after) != 0 {
		t.Fatal("a later sample did not observe the committed source deactivation", after, err)
	}
}

func TestRandomAlbumsRechecksSessionInTheSameSQLiteSnapshot(t *testing.T) {
	f := albumSelectionFixture(t)
	tx, err := f.svc.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	actor, err := f.svc.auth.SessionInTx(t.Context(), tx, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.db.ExecContext(t.Context(), `UPDATE users SET active=0 WHERE id=?`, actor.User.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(t.Context(), randomAlbumsQuery, actor.User.ID, MaxRandomAlbums, actor.User.ID, actor.User.ID, actor.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		if _, err := scanAlbum(rows); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if count == 0 {
		t.Fatal("already-authorized read snapshot changed after the later revoke")
	}
	if _, err := f.svc.RandomAlbums(t.Context(), f.base, MaxRandomAlbums); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatal("a new read failed to observe account revocation", err)
	}
}
