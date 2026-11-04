package media

import (
	"database/sql"
	"testing"

	"gallerygo/internal/auth"
)

func TestOriginalRequiresExactRootMarkerPinAndActiveSource(t *testing.T) {
	f := fixture(t)
	content, err := f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	if err != nil {
		t.Fatal(err)
	}
	content.Close()
	// A root check alone must not make an old inventory trustworthy after an
	// operator changes configuration, even if the new path has matching stat data.
	if _, err := f.db.Exec(`UPDATE sources SET config_version=2 WHERE id='source'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	requireError(t, err, ErrSourceUnavailable)
	if _, err := f.db.Exec(`UPDATE sources SET config_version=1 WHERE id='source'; UPDATE source_entries SET source_config_version=0 WHERE id='entry'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
	requireError(t, err, ErrSourceUnavailable)
	if _, err := f.db.Exec(`UPDATE source_entries SET source_config_version=1 WHERE id='entry'`); err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{"", (SourceRoot{Path: f.source, Marker: ".different-marker"}).Signature()} {
		if _, err := f.db.Exec(`UPDATE sources SET root_config_sha256=? WHERE id='source'`, pin); err != nil {
			t.Fatal(err)
		}
		_, err := f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
		requireError(t, err, ErrSourceUnavailable)
		// An unavailable root does not revoke an otherwise authorized ready cache.
		preview, err := f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
		if err != nil {
			t.Fatal("root pin failure removed cached preview access", err)
		}
		preview.Close()
	}
	if _, err := f.db.Exec(`UPDATE sources SET root_config_sha256=?,is_active=0 WHERE id='source'`, (SourceRoot{Path: f.source, Marker: ".mount-marker"}).Signature()); err != nil {
		t.Fatal(err)
	}
	for _, viewer := range []auth.Session{f.owner, f.reader} {
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
}
