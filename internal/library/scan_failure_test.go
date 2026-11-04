package library

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"gallerygo/internal/store"
)

func TestScanFailureRetainsPublishedEvidenceAndMonotonicHealth(t *testing.T) {
	f := sourceSetup(t)
	source := f.create(t, "photos")
	source, err := f.service.Check(t.Context(), f.owner, source.ID, source.Version, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	now := source.LastCheckedAt.Add(-time.Hour)
	f.service.now = func() time.Time { return now }
	if _, err := f.db.Exec(`UPDATE sources SET scan_generation=7,last_full_scan_at=? WHERE id=?`, source.LastCheckedAt.UnixMilli(), source.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.service.RecordScanFailureTx(t.Context(), tx, source.ID, source.ConfigVersion, "MINIMUM_FILES"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := f.service.Get(t.Context(), f.owner, source.ID)
	if err != nil || current.State != "error" || current.ScanGeneration != 7 || current.ConfigVersion != source.ConfigVersion || current.Version != source.Version+1 || current.LastErrorCode == nil || *current.LastErrorCode != "MINIMUM_FILES" || !current.LastCheckedAt.Equal(*source.LastCheckedAt) || !current.LastHealthyAt.Equal(*source.LastHealthyAt) || !current.LastFullScanAt.Equal(*source.LastCheckedAt) || !current.UpdatedAt.Equal(source.UpdatedAt) {
		t.Fatal("failure rewrote authoritative evidence", current, err)
	}
}

func TestScanFailureRejectsUnsafeCodesStaleEpochsInactiveOrMissingSources(t *testing.T) {
	f := sourceSetup(t)
	source := f.create(t, "photos")
	cases := []struct {
		id     string
		config int64
		code   string
		want   error
	}{
		{source.ID, source.ConfigVersion, "/private/source/path", ErrSourceSettings},
		{source.ID, source.ConfigVersion + 1, "IO_ERROR", ErrSourceChanged},
		{store.NewID(), source.ConfigVersion, "IO_ERROR", sql.ErrNoRows},
	}
	for _, c := range cases {
		tx, err := f.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = f.service.RecordScanFailureTx(t.Context(), tx, c.id, c.config, c.code)
		tx.Rollback()
		if !errors.Is(err, c.want) {
			t.Fatal(c, err)
		}
	}
	if _, err := f.db.Exec(`UPDATE sources SET is_active=0 WHERE id=?`, source.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.service.RecordScanFailureTx(t.Context(), tx, source.ID, source.ConfigVersion, "SOURCE_UNAVAILABLE"); !errors.Is(err, ErrSourceChanged) {
		t.Fatal("inactive source gained failure publication authority", err)
	}
}
