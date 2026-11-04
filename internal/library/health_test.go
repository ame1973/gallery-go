package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/auth"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func TestReachabilityChecksPreserveIndexesAndNeverCreateMissingRoots(t *testing.T) {
	f := sourceSetup(t)
	missing := f.create(t, "unmounted")
	if _, err := os.Stat(f.roots["unmounted"].Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("creation touched an unmounted root", err)
	}
	unavailable, err := f.service.Check(t.Context(), f.owner, missing.ID, missing.Version, store.NewID())
	if err != nil || unavailable.State != "unavailable" || unavailable.LastErrorCode == nil || *unavailable.LastErrorCode != "SOURCE_UNAVAILABLE" || unavailable.ConfigVersion != missing.ConfigVersion {
		t.Fatal(unavailable, err)
	}
	if _, err := os.Stat(f.roots["unmounted"].Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("probe created an unmounted root", err)
	}
	input := DefaultSourceSettings()
	input.LibraryID, input.RootName, input.MinimumFiles = f.first.ID, "photos", 10
	source, err := f.service.Create(t.Context(), f.owner, input, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,display_name,created_at,updated_at) VALUES('preserved',?,?,'Source title','User override',0,0)`, f.first.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,updated_at) VALUES('preserved-entry',?,'absent-photo.png',10,0,0)`, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorites(user_id,album_id,created_at) VALUES(?,'preserved',0)`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	key := store.NewID()
	healthy, err := f.service.Check(t.Context(), f.owner, source.ID, source.Version, key)
	if err != nil || healthy.State != "healthy" || healthy.LastHealthyAt == nil || healthy.ScanGeneration != 0 || healthy.LastFullScanAt != nil || healthy.ConfigVersion != source.ConfigVersion {
		t.Fatal("reachability pretended to publish inventory", healthy, err)
	}
	marker := filepath.Join(f.roots["photos"].Path, ".mounted")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	replay, err := f.service.Check(t.Context(), f.owner, source.ID, source.Version, key)
	if err != nil || replay.Version != healthy.Version || replay.State != "healthy" {
		t.Fatal("probe replay repeated filesystem work", replay, err)
	}
	failed, err := f.service.Check(t.Context(), f.owner, source.ID, healthy.Version, store.NewID())
	if err != nil || failed.State != "unavailable" || failed.LastHealthyAt == nil || !failed.LastHealthyAt.Equal(*healthy.LastHealthyAt) {
		t.Fatal(failed, err)
	}
	var retained int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM favorites f JOIN albums a ON a.id=f.album_id JOIN source_entries e ON e.source_id=a.source_id WHERE a.id='preserved' AND a.display_name='User override' AND e.availability='present'`).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("source outage changed indexed metadata or favorites", retained, err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("probe recreated missing mount marker", err)
	}
}

func TestRootProbeRejectsEscapeNonRegularMarkersAndCancelledContexts(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "private-marker")
	if err := os.WriteFile(outside, []byte("outside"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := ProbeRoot(t.Context(), media.SourceRoot{Path: root, Marker: "escape"}); err == nil {
		t.Fatal("mount marker escaped source confinement")
	}
	for _, source := range []media.SourceRoot{{Path: "", Marker: "marker"}, {Path: root, Marker: "../private-marker"}, {Path: root, Marker: "."}, {Path: root, Marker: "bad\x00marker"}} {
		sourceError(t, ProbeRoot(t.Context(), source), ErrUnknownRoot)
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- ProbeRoot(t.Context(), media.SourceRoot{Path: root, Marker: "fifo"}) }()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("FIFO mount marker accepted")
		}
	case <-time.After(2 * time.Second):
		if writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			writer.Close()
		}
		t.Fatal("opening a mount marker FIFO blocked")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sourceError(t, ProbeRoot(ctx, media.SourceRoot{Path: root, Marker: "missing"}), context.Canceled)
}

func TestMappingSupportsArbitraryFolderDepthAndRootFiles(t *testing.T) {
	for _, test := range []struct {
		mapping  Mapping
		relative string
		want     Location
	}{
		{Mapping{2, 1}, "Alice/Trip/deeper/photo.png", Location{"Alice/Trip", "Trip", "Alice", "Alice"}},
		{Mapping{3, 2}, "Region/Alice/Trip/photo.png", Location{"Region/Alice/Trip", "Trip", "Alice", "Region/Alice"}},
		{Mapping{0, 0}, "Alice/Trip/photo.png", Location{".", "Photos", "", ""}},
		{Mapping{3, 1}, "Alice/photo.png", Location{"Alice", "Alice", "Alice", "Alice"}},
		{Mapping{2, 1}, "photo.png", Location{".", "Photos", "", ""}},
		{Mapping{2, 1}, "人物/旅行/photo.png", Location{"人物/旅行", "旅行", "人物", "人物"}},
	} {
		got, err := test.mapping.Locate(test.relative, "Photos")
		if err != nil || got != test.want {
			t.Fatal(test.relative, got, test.want, err)
		}
	}
	for _, relative := range []string{".", "/absolute.jpg", "../escape.jpg", "dir/../escape.jpg", "dir//photo.jpg", "dir/./photo.jpg", "dir\\photo.jpg", "bad\x00photo.jpg"} {
		_, err := DefaultMapping().Locate(relative, "Photos")
		sourceError(t, err, ErrSourceSettings)
	}
	for _, mapping := range []Mapping{{-1, 0}, {17, 0}, {1, -1}, {1, 2}} {
		sourceError(t, mapping.Validate(), ErrSourceSettings)
	}
}

func TestSourceSettingsRegistryPaginationAndConfigurationFences(t *testing.T) {
	f := sourceSetup(t)
	for _, change := range []func(*SourceSettings){
		func(s *SourceSettings) { s.MinimumFiles = -1 },
		func(s *SourceSettings) { s.PollIntervalSeconds = 9 },
		func(s *SourceSettings) { s.FullScanIntervalSeconds = 1 },
		func(s *SourceSettings) { s.FullScanIntervalSeconds = 604801 },
		func(s *SourceSettings) { s.Mapping.PersonDepth = 2 },
	} {
		settings := DefaultSourceSettings()
		settings.LibraryID, settings.RootName = f.first.ID, "photos"
		change(&settings)
		_, err := f.service.Create(t.Context(), f.owner, settings, store.NewID())
		sourceError(t, err, ErrSourceSettings)
	}
	settings := DefaultSourceSettings()
	settings.LibraryID, settings.RootName = f.first.ID, "https://untrusted.example/media"
	_, err := f.service.Create(t.Context(), f.owner, settings, store.NewID())
	sourceError(t, err, ErrUnknownRoot)
	roots, err := f.service.Roots(t.Context(), f.owner, "", 1)
	if err != nil || len(roots) != 1 || roots[0].Name != "photos" {
		t.Fatal(roots, err)
	}
	next, err := f.service.Roots(t.Context(), f.owner, roots[0].Name, 101)
	if err != nil || len(next) != 2 || next[0].Name != "replacement" {
		t.Fatal(next, err)
	}
	for _, limit := range []int{0, 102} {
		_, err := f.service.Roots(t.Context(), f.owner, "", limit)
		sourceError(t, err, ErrSourceSettings)
		_, err = f.service.List(t.Context(), f.owner, "", limit)
		sourceError(t, err, ErrSourceSettings)
	}
	source := f.create(t, "photos")
	other := f.create(t, "replacement")
	page, err := f.service.List(t.Context(), f.owner, "", 1)
	if err != nil || len(page) != 1 {
		t.Fatal(page, err)
	}
	nextPage, err := f.service.List(t.Context(), f.owner, page[0].ID, 1)
	if err != nil || len(nextPage) != 1 || nextPage[0].ID == page[0].ID {
		t.Fatal(nextPage, err)
	}
	_, err = f.service.Update(t.Context(), f.owner, source.ID, source.Version, SourcePatch{RootName: &other.RootName})
	sourceError(t, err, ErrRootInUse)
	_, err = f.service.Update(t.Context(), f.owner, source.ID, source.Version, SourcePatch{})
	sourceError(t, err, ErrSourceSettings)
	_, err = f.service.Update(t.Context(), f.owner, source.ID, 99, SourcePatch{Active: sourcePointer(false)})
	sourceError(t, err, auth.ErrConflict)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	checked, root, err := f.service.RequireConfigTx(t.Context(), tx, source.ID, source.ConfigVersion)
	if err != nil || checked.ID != source.ID || root.Path != source.rootPath {
		t.Fatal(checked, root, err)
	}
}
