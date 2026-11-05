package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

const fixturePassword = "a private protected media test passphrase"

type mediaFixture struct {
	svc       *Service
	auth      *auth.Service
	access    *access.Service
	db        *sql.DB
	owner     auth.Session
	reader    auth.Session
	cache     string
	source    string
	original  string
	variant   string
	storage   string
	data      []byte
	hash      string
	mtime     int64
	mediaID   string
	albumID   string
	libraryID string
}

func fixture(t *testing.T) mediaFixture {
	t.Helper()
	dir := t.TempDir()
	cache, source := filepath.Join(dir, "cache"), filepath.Join(dir, "source")
	for _, directory := range []string{cache, source, filepath.Join(source, "images")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, ".mount-marker"), []byte("isolated test mount"), 0o600); err != nil {
		t.Fatal(err)
	}
	photo := image.NewRGBA(image.Rect(0, 0, 8, 8))
	photo.Set(1, 1, color.RGBA{R: 30, G: 120, B: 180, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, photo); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	original := filepath.Join(source, "images", "fixture.png")
	if err := os.WriteFile(original, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(original)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := VariantPath("revision", "fixture_png", 1, hash, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	variant := filepath.Join(cache, filepath.FromSlash(storage))
	if err := os.MkdirAll(filepath.Dir(variant), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(variant, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(dir, "data", "media.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Bootstrap(t.Context(), "owner", fixturePassword); err != nil {
		t.Fatal(err)
	}
	owner, err := a.Login(t.Context(), "owner", fixturePassword, "192.0.2.1", "media owner", "")
	if err != nil {
		t.Fatal(err)
	}
	user, err := a.CreateUser(t.Context(), owner, auth.NewUser{Username: "reader", Password: fixturePassword, Role: "user", Active: true, DownloadOriginal: true}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := a.Login(t.Context(), "reader", fixturePassword, "192.0.2.2", "media reader", "")
	if err != nil {
		t.Fatal(err)
	}
	queries := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO permission_groups(id,name,created_at) VALUES('group','Fixture',0)`, nil},
		{`INSERT INTO libraries(id,name,created_at) VALUES('library','Fixture',0)`, nil},
		{`INSERT INTO sources(id,library_id,name,root_path,root_config_sha256,mapping_json,state,created_at) VALUES('source','library','fixture',?,?,'{}','healthy',0)`, []any{source, (SourceRoot{Path: source, Marker: ".mount-marker"}).Signature()}},
		{`INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES('album','library','source','Fixture',0,0)`, nil},
		{`INSERT INTO user_groups(user_id,group_id) VALUES(?,'group')`, []any{user.ID}},
		{`INSERT INTO library_access(library_id,group_id) VALUES('library','group')`, nil},
		{`INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,sha256,source_config_version,updated_at) VALUES('entry','source','images/fixture.png',?,?,?,1,0)`, []any{len(data), info.ModTime().UnixNano(), hash}},
		{`INSERT INTO media(id,album_id,source_entry_id,kind,file_name,mime,width,height,current_revision_id,created_at,updated_at) VALUES('media','album','entry','image','fixture.png','image/png',8,8,'revision',0,0)`, nil},
		{`INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) VALUES('revision','media',?,?,?,1,0)`, []any{hash, len(data), info.ModTime().UnixNano()}},
		{`INSERT INTO media_variants(id,revision_id,preset_key,preset_version,storage_path,mime,size_bytes,sha256,width,height,status,created_at) VALUES('variant','revision','fixture_png',1,?,'image/png',?,?,8,8,'ready',0)`, []any{storage, len(data), hash}},
	}
	for _, query := range queries {
		if _, err := db.Exec(query.sql, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	return mediaFixture{New(db, a, cache, map[string]SourceRoot{"fixture": {Path: source, Marker: ".mount-marker"}}), a, access.New(db, a), db, owner, reader, cache, source, original, variant, storage, data, hash, info.ModTime().UnixNano(), "media", "album", "library"}
}

func requireError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v want=%v", err, want)
	}
}

func TestMetadataOrientationAndCaptureUseTheSamePrivateVisibility(t *testing.T) {
	f := fixture(t)
	capture := time.Date(2026, 11, 5, 4, 34, 56, 0, time.UTC)
	if _, err := f.db.Exec(`UPDATE media SET orientation=6,capture_at=? WHERE id=?`, capture.UnixMilli(), f.mediaID); err != nil {
		t.Fatal(err)
	}
	value, err := f.svc.Get(t.Context(), f.reader, f.mediaID)
	if err != nil || value.Orientation == nil || *value.Orientation != 6 || value.CaptureAt == nil || !value.CaptureAt.Equal(capture) || value.CaptureAt.Location() != time.UTC {
		t.Fatal(value, err)
	}
	items, err := f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, "", 30)
	if err != nil || len(items) != 1 || items[0].Orientation == nil || *items[0].Orientation != 6 || items[0].CaptureAt == nil || !items[0].CaptureAt.Equal(capture) {
		t.Fatal(items, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil || !bytes.Contains(encoded, []byte(`"capture_at":"2026-11-05T04:34:56Z"`)) || bytes.Contains(encoded, []byte("GPS")) || bytes.Contains(encoded, []byte(f.source)) {
		t.Fatal(string(encoded), err)
	}
	if _, err := f.db.Exec(`UPDATE media SET orientation=NULL,capture_at=NULL WHERE id=?`, f.mediaID); err != nil {
		t.Fatal(err)
	}
	value, err = f.svc.Get(t.Context(), f.reader, f.mediaID)
	if err != nil || value.Orientation != nil || value.CaptureAt != nil {
		t.Fatal("unknown capture fell back to mtime", value, err)
	}
	if _, err := f.db.Exec(`DELETE FROM library_access`); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Get(t.Context(), f.reader, f.mediaID)
	requireError(t, err, sql.ErrNoRows)
}

func TestMetadataAndCurrentReadyContentAreRealAndReadOnly(t *testing.T) {
	f := fixture(t)
	metadata, err := f.svc.Get(t.Context(), f.reader, f.mediaID)
	if err != nil || metadata.Revision == nil || *metadata.Revision != "revision" || metadata.Width == nil || *metadata.Width != 8 || metadata.OfflineCapable {
		t.Fatalf("metadata: %+v %v", metadata, err)
	}
	items, err := f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, "", 30)
	if err != nil || len(items) != 1 || items[0].ID != f.mediaID {
		t.Fatal("media list/detail disagree", items, err)
	}
	items, err = f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, f.mediaID, 30)
	if err != nil || len(items) != 0 {
		t.Fatal("media keyset repeated item", err)
	}
	variants, err := f.svc.Variants(t.Context(), f.reader, f.mediaID, "", 30)
	if err != nil || len(variants) != 1 || variants[0].SHA256 != f.hash || variants[0].ContentURL != "/api/v2/media/media/variants/variant/content" {
		t.Fatalf("variants: %+v %v", variants, err)
	}
	payload, _ := json.Marshal(variants)
	for _, private := range []string{f.cache, f.source, f.storage, "storage_path"} {
		if strings.Contains(string(payload), private) {
			t.Fatal("metadata leaked private path")
		}
	}
	before, err := os.Stat(f.original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.original, 0o400); err != nil {
		t.Fatal(err)
	}
	for _, original := range []bool{false, true} {
		var content *Content
		if original {
			content, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
		} else {
			content, err = f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(content)
		if err != nil || !bytes.Equal(got, f.data) || content.MIME != "image/png" || content.Attachment != original || content.ETag != `"sha256-`+f.hash+`"` {
			t.Fatalf("content bytes/headers do not match fixture: %v %+v", err, content)
		}
		if _, err := content.Seek(1, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got, err = io.ReadAll(content)
		if err != nil || !bytes.Equal(got, f.data[1:]) {
			t.Fatal("seek did not return the expected bytes", err)
		}
		content.Close()
	}
	after, err := os.Stat(f.original)
	if err != nil || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("source changed during read", err)
	}
	contents, err := os.ReadFile(f.original)
	if err != nil || !bytes.Equal(contents, f.data) {
		t.Fatal("source content was rewritten", err)
	}
}

func TestMediaAuthorizationIgnoresForgedDTOAndAppliesOriginalPolicyToAdmins(t *testing.T) {
	f := fixture(t)
	for _, id := range []string{"unknown", "' OR 1=1 --"} {
		_, err := f.svc.Get(t.Context(), f.reader, id)
		requireError(t, err, sql.ErrNoRows)
	}
	if _, err := f.db.Exec(`UPDATE users SET download_original=0 WHERE id IN (?,?)`, f.owner.User.ID, f.reader.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, viewer := range []auth.Session{f.owner, f.reader} {
		viewer.User.DownloadOriginal = true
		_, err := f.svc.OpenOriginal(t.Context(), viewer, f.mediaID)
		requireError(t, err, sql.ErrNoRows)
		content, err := f.svc.OpenVariant(t.Context(), viewer, f.mediaID, "variant")
		if err != nil {
			t.Fatal("preview needs view permission, not original policy", err)
		}
		content.Close()
	}
	if _, err := f.db.Exec(`DELETE FROM library_access`); err != nil {
		t.Fatal(err)
	}
	forged := f.reader
	forged.User = f.owner.User
	checks := []func(auth.Session) error{
		func(viewer auth.Session) error { _, e := f.svc.Get(t.Context(), viewer, f.mediaID); return e },
		func(viewer auth.Session) error {
			_, e := f.svc.AlbumMedia(t.Context(), viewer, f.albumID, "", 30)
			return e
		},
		func(viewer auth.Session) error {
			_, e := f.svc.Variants(t.Context(), viewer, f.mediaID, "", 30)
			return e
		},
		func(viewer auth.Session) error {
			c, e := f.svc.OpenVariant(t.Context(), viewer, f.mediaID, "variant")
			if c != nil {
				c.Close()
			}
			return e
		},
		func(viewer auth.Session) error {
			c, e := f.svc.OpenOriginal(t.Context(), viewer, f.mediaID)
			if c != nil {
				c.Close()
			}
			return e
		},
	}
	for _, check := range checks {
		requireError(t, check(forged), sql.ErrNoRows)
		requireError(t, check(auth.Session{}), auth.ErrUnauthenticated)
	}
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	for _, check := range checks {
		requireError(t, check(f.owner), auth.ErrUnauthenticated)
	}
}

func TestOnlyReadyCurrentRevisionVariantsCanBeRead(t *testing.T) {
	f := fixture(t)
	for _, status := range []string{"failed", "retired"} {
		if _, err := f.db.Exec(`UPDATE media_variants SET status=? WHERE id='variant'`, status); err != nil {
			t.Fatal(err)
		}
		_, err := f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
		requireError(t, err, sql.ErrNoRows)
		items, err := f.svc.Variants(t.Context(), f.reader, f.mediaID, "", 30)
		if err != nil || len(items) != 0 {
			t.Fatal("non-ready variant advertised", items, err)
		}
	}
	if _, err := f.db.Exec(`UPDATE media_variants SET status='ready'; UPDATE media SET current_revision_id=NULL`); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
	requireError(t, err, sql.ErrNoRows)
	if _, err := f.db.Exec(`UPDATE media SET current_revision_id='revision'; INSERT INTO media(id,album_id,kind,file_name,created_at,updated_at) VALUES('other','album','image','other.png',0,0)`); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.OpenVariant(t.Context(), f.reader, "other", "variant")
	requireError(t, err, sql.ErrNoRows)
	if _, err := f.db.Exec(`UPDATE media_revisions SET media_id='other' WHERE id='revision'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
	requireError(t, err, sql.ErrNoRows)
	_, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	requireError(t, err, sql.ErrNoRows)
}

func TestVariantStorageCannotEscapeOrUseInconsistentMetadata(t *testing.T) {
	f := fixture(t)
	for _, storage := range []string{"../secret.png", "/secret.png", ".", `variants\revision\secret.png`, "variants/other/fixture_png/1/" + f.hash + ".png"} {
		if _, err := f.db.Exec(`UPDATE media_variants SET storage_path=? WHERE id='variant'`, storage); err != nil {
			t.Fatal(err)
		}
		_, err := f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
		requireError(t, err, ErrVariantUnavailable)
	}
	if _, err := f.db.Exec(`UPDATE media_variants SET storage_path=?,mime='text/html' WHERE id='variant'`, f.storage); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
	requireError(t, err, ErrVariantUnavailable)
	if _, err := f.db.Exec(`UPDATE media_variants SET mime='image/png',size_bytes=size_bytes+1 WHERE id='variant'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
	requireError(t, err, ErrVariantUnavailable)
	if _, err := f.db.Exec(`UPDATE media_variants SET size_bytes=?,sha256='invalid' WHERE id='variant'`, len(f.data)); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
	requireError(t, err, ErrVariantUnavailable)
	if _, err := f.db.Exec(`UPDATE media_variants SET sha256=? WHERE id='variant'`, f.hash); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.variant); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private.png")
	if err := os.WriteFile(outside, f.data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, f.variant); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
	requireError(t, err, ErrVariantUnavailable)
}

func TestSourceHealthRegistryMarkerPathsAndRevisionSignaturesFailClosed(t *testing.T) {
	f := fixture(t)
	checks := []struct {
		sql         string
		args        []any
		restore     string
		restoreArgs []any
	}{
		{`UPDATE sources SET state='unavailable'`, nil, `UPDATE sources SET state='healthy'`, nil},
		{`UPDATE sources SET name='unconfigured'`, nil, `UPDATE sources SET name='fixture'`, nil},
		{`UPDATE sources SET root_path='/not-configured'`, nil, `UPDATE sources SET root_path=?`, []any{f.source}},
		{`UPDATE source_entries SET availability='quarantined'`, nil, `UPDATE source_entries SET availability='present'`, nil},
		{`UPDATE source_entries SET size_bytes=size_bytes+1`, nil, `UPDATE source_entries SET size_bytes=?`, []any{len(f.data)}},
		{`UPDATE source_entries SET mtime_ns=mtime_ns+1`, nil, `UPDATE source_entries SET mtime_ns=?`, []any{f.mtime}},
		{`UPDATE source_entries SET sha256=NULL`, nil, `UPDATE source_entries SET sha256=?`, []any{f.hash}},
		{`UPDATE source_entries SET relative_path='../private.png'`, nil, `UPDATE source_entries SET relative_path='images/fixture.png'`, nil},
	}
	for _, check := range checks {
		if _, err := f.db.Exec(check.sql, check.args...); err != nil {
			t.Fatal(err)
		}
		_, err := f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
		requireError(t, err, ErrSourceUnavailable)
		if _, err := f.db.Exec(check.restore, check.restoreArgs...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(f.source, ".mount-marker")); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	requireError(t, err, ErrSourceUnavailable)
	cached, err := f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
	if err != nil {
		t.Fatal("source outage must not remove an authorized ready cache variant", err)
	}
	cached.Close()
}

func TestSourceSymlinksSpecialFilesAndChangesDuringReads(t *testing.T) {
	f := fixture(t)
	outside := filepath.Join(t.TempDir(), "private.png")
	if err := os.WriteFile(outside, f.data, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.source, "images", "link.png")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE source_entries SET relative_path='images/link.png'`); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	requireError(t, err, ErrSourceUnavailable)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("fixture.png", link); err != nil {
		t.Fatal(err)
	}
	content, err := f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	if err != nil {
		t.Fatal("a confined relative symlink should be usable", err)
	}
	content.Close()
	fifo := filepath.Join(f.source, "images", "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE source_entries SET relative_path='images/fifo'`); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		c, e := f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
		if c != nil {
			c.Close()
		}
		result <- e
	}()
	select {
	case err := <-result:
		requireError(t, err, ErrSourceUnavailable)
	case <-time.After(2 * time.Second):
		if writer, e := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); e == nil {
			writer.Close()
		}
		t.Fatal("opening a FIFO blocked instead of rejecting it")
	}
	if _, err := f.db.Exec(`UPDATE source_entries SET relative_path='images/fixture.png'`); err != nil {
		t.Fatal(err)
	}
	content, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	if err := os.WriteFile(f.original, []byte("changed source"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = content.Read(make([]byte, 32))
	requireError(t, err, ErrContentChanged)
	requireError(t, content.Err(), ErrContentChanged)
	_, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	requireError(t, err, ErrSourceUnavailable)
}

func TestGrantRevocationAndSourceReassignmentHideEveryMediaRead(t *testing.T) {
	f := fixture(t)
	membership, err := f.access.UserGroups(t.Context(), f.owner, f.reader.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.SetUserGroups(t.Context(), f.owner, f.reader.User.ID, membership.Version, []string{}); err != nil {
		t.Fatal(err)
	}
	assertHidden := func(viewer auth.Session) {
		t.Helper()
		_, err := f.svc.Get(t.Context(), viewer, f.mediaID)
		requireError(t, err, sql.ErrNoRows)
		_, err = f.svc.AlbumMedia(t.Context(), viewer, f.albumID, "", 30)
		requireError(t, err, sql.ErrNoRows)
		_, err = f.svc.Variants(t.Context(), viewer, f.mediaID, "", 30)
		requireError(t, err, sql.ErrNoRows)
		_, err = f.svc.OpenVariant(t.Context(), viewer, f.mediaID, "variant")
		requireError(t, err, sql.ErrNoRows)
		_, err = f.svc.OpenOriginal(t.Context(), viewer, f.mediaID)
		requireError(t, err, sql.ErrNoRows)
	}
	assertHidden(f.reader)
	membership, err = f.access.UserGroups(t.Context(), f.owner, f.reader.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.access.SetUserGroups(t.Context(), f.owner, f.reader.User.ID, membership.Version, []string{"group"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('other-library','Other',0); UPDATE sources SET library_id='other-library'`); err != nil {
		t.Fatal(err)
	}
	assertHidden(f.reader)
	assertHidden(f.owner)
}

func TestMediaCancellationAndConfigurationAreNotFallbackCapabilities(t *testing.T) {
	f := fixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	content, err := f.svc.OpenVariant(ctx, f.reader, f.mediaID, "variant")
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	cancel()
	_, err = content.Read(make([]byte, 8))
	requireError(t, err, context.Canceled)
	_, err = content.Seek(0, io.SeekStart)
	requireError(t, err, context.Canceled)
	unconfigured := New(f.db, f.auth, "", nil)
	_, err = unconfigured.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
	requireError(t, err, ErrVariantUnavailable)
	_, err = unconfigured.OpenOriginal(t.Context(), f.reader, f.mediaID)
	requireError(t, err, ErrSourceUnavailable)
	for _, limit := range []int{0, 102} {
		_, err := f.svc.AlbumMedia(t.Context(), f.reader, f.albumID, "", limit)
		requireError(t, err, access.ErrGroupSet)
		_, err = f.svc.Variants(t.Context(), f.reader, f.mediaID, "", limit)
		requireError(t, err, access.ErrGroupSet)
	}
}
