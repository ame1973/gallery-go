package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func TestScanHandlerWaitsForLiveSourceOwnerAndObservesShutdown(t *testing.T) {
	f := inventorySetup(t)
	writeInput(t, media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}, "photo.png", publicationPNG(t, 12))
	owner := f.claim(t, "scan:held-source")
	generation, err := f.inventory.Begin(t.Context(), owner, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil {
		t.Fatal(err)
	}
	scans := NewScans(f.db, f.auth, f.queue)
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil || lease.Job.ID != job.ID {
		t.Fatal(lease, err)
	}
	handler, err := scans.Handler(f.inventory, media.DefaultInspectPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	// The other source owner stays live; waiting must not steal its generation,
	// fabricate progress, or use a retry attempt merely to poll this mutex.
	_, err = scans.executeSource(ctx, lease, f.inventory, media.DefaultInspectPolicy(), scanTarget{sourceID: f.source.ID, config: f.source.ConfigVersion, scope: "."}, "full", true)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() == nil {
		t.Fatal("live source ownership was bypassed", err)
	}
	current, err := f.inventory.Get(t.Context(), generation.ID)
	if err != nil || current.State != "walking" || current.Version != generation.Version {
		t.Fatal(current, err)
	}
	result, err := f.queue.Get(t.Context(), job.ID)
	if err != nil || result.Done != 0 || result.Attempts != 1 {
		t.Fatal("waiting fabricated work", result, err)
	}
	stopped, stop := context.WithCancel(t.Context())
	stop()
	if err := handler(stopped, lease); !errors.Is(err, context.Canceled) {
		t.Fatal("handler ignored shutdown", err)
	}
	if err := f.queue.Fail(t.Context(), owner, "WORKER_STOPPED", false); err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), lease); err != nil {
		t.Fatal("new owner could not supersede abandoned staging", err)
	}
	result, err = f.queue.Get(t.Context(), job.ID)
	if err != nil || result.Status != "succeeded" || result.Attempts != 1 {
		t.Fatal(result, err)
	}
	current, err = f.inventory.Get(t.Context(), generation.ID)
	if err != nil || current.State != "superseded" {
		t.Fatal(current, err)
	}
}

func TestScanHandlerConfigurationCancellationAndLeaseExpiryFenceExecution(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	handler, err := scans.Handler(f.inventory, media.DefaultInspectPolicy())
	if err != nil {
		t.Fatal(err)
	}
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	minimum := int64(1)
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{MinimumFiles: &minimum})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), lease); !errors.Is(err, jobs.ErrCancelled) {
		t.Fatal("changed config still executed", err)
	}
	if err := f.queue.AcknowledgeCancel(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	result, err := f.queue.Get(t.Context(), job.ID)
	if err != nil || result.Status != "cancelled" || result.Done != 0 {
		t.Fatal(result, err)
	}
	job, err = scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	lease, err = f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE jobs SET lease_expires_at=0 WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), lease); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("expired lease still executed", err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inventory_generations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("revoked execution touched staging", count, err)
	}
}

func TestScanHandlerUnconfiguredRegistryFailsWithoutAdoptingAnotherRoot(t *testing.T) {
	f := inventorySetup(t)
	other := walkRoot(t)
	writeInput(t, other, "photo.png", publicationPNG(t, 12))
	// A second process has the same declared name but a different private pin.
	// It must record an unavailable source, never adopt or scan the new tree.
	f.sources = library.NewSources(f.db, f.auth, f.queue, map[string]media.SourceRoot{"fixture": other})
	f.inventory.sources = f.sources
	scans := NewScans(f.db, f.auth, f.queue)
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	startScanExecutor(t, f, scans)
	result := waitScanResult(t, f, job.ID)
	if result.Status != "dead" || result.Failed != 3 {
		t.Fatal("registry mismatch became success", result)
	}
	current, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil || current.Configured || current.State != "unavailable" || current.LastErrorCode == nil || *current.LastErrorCode != "SOURCE_NOT_CONFIGURED" || current.ScanGeneration != 0 {
		t.Fatal(current, err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media)+(SELECT COUNT(*) FROM inventory_generations)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("obsolete registry was adopted", count, err)
	}
}

func TestScanHandlerNativeFailuresRemainFailedAndNonMediaSkipped(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "Album/good.png", publicationPNG(t, 12))
	writeInput(t, root, "Album/bad.jpg", []byte("not a valid JPEG"))
	writeInput(t, root, "notes.txt", []byte("not media"))
	scans := NewScans(f.db, f.auth, f.queue)
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "incremental"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	startScanExecutor(t, f, scans)
	result := waitScanResult(t, f, job.ID)
	if result.Status != "failed" || result.Attempts != 1 || result.Failed != 1 || result.Skipped != 1 || result.Succeeded != 7 || result.Done != 9 || !result.TotalKnown {
		t.Fatal("native errors became success or non-media became failed", result, result.Counts)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM media`).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

func TestScanHandlerAlbumTargetPublishesOnlyThePinnedScope(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "A/photo.png", publicationPNG(t, 12))
	writeInput(t, root, "B/photo.png", publicationPNG(t, 13))
	f.publish(t, "full", ".")
	var album, otherRevision string
	if err := f.db.QueryRow(`SELECT (SELECT id FROM albums WHERE relative_path='A'),(SELECT m.current_revision_id FROM media m JOIN source_entries e ON e.id=m.source_entry_id WHERE e.relative_path='B/photo.png')`).Scan(&album, &otherRevision); err != nil {
		t.Fatal(err)
	}
	for path, width := range map[string]int{"A/photo.png": 14, "B/photo.png": 15} {
		replacement := writeInput(t, root, "replacement.png", publicationPNG(t, width))
		if err := os.Rename(replacement, filepath.Join(root.Path, filepath.FromSlash(path))); err != nil {
			t.Fatal(err)
		}
	}
	scans := NewScans(f.db, f.auth, f.queue)
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{AlbumID: &album, Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	handler, err := scans.Handler(f.inventory, media.DefaultInspectPolicy())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	result, err := f.queue.Get(t.Context(), job.ID)
	if err != nil || result.Status != "succeeded" {
		t.Fatal(result, result.Counts, err)
	}
	var width int64
	var revision string
	if err := f.db.QueryRow(`SELECT (SELECT m.width FROM media m JOIN source_entries e ON e.id=m.source_entry_id WHERE e.relative_path='A/photo.png'),(SELECT m.current_revision_id FROM media m JOIN source_entries e ON e.id=m.source_entry_id WHERE e.relative_path='B/photo.png')`).Scan(&width, &revision); err != nil || width != 14 || revision != otherRevision {
		t.Fatal("target published outside its scope", width, revision, err)
	}
	target := scanTarget{sourceID: f.source.ID, config: f.source.ConfigVersion, scope: "wrong"}
	target.album.String, target.album.Valid = album, true
	// Use a new live lease; the old completed job never grants execution authority.
	if _, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "incremental"}, store.NewID()); err != nil {
		t.Fatal(err)
	}
	lease, err = f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scans.executeSource(t.Context(), lease, f.inventory, media.DefaultInspectPolicy(), target, "full", true); !errors.Is(err, library.ErrSourceChanged) {
		t.Fatal("changed album scope was followed", err)
	}
}

func TestScanFailureFinalLeaseFenceRollsBackAllDiagnostics(t *testing.T) {
	f := inventorySetup(t)
	if err := os.Remove(filepath.Join(f.rootPath, ".mounted")); err != nil {
		t.Fatal(err)
	}
	scans := NewScans(f.db, f.auth, f.queue)
	handler, err := scans.Handler(f.inventory, media.DefaultInspectPolicy())
	if err != nil {
		t.Fatal(err)
	}
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER expire_scan_report AFTER INSERT ON audit_events WHEN NEW.event_type='scan_target_failed' BEGIN UPDATE jobs SET lease_expires_at=0 WHERE id=NEW.resource_id; END`); err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), lease); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("failure publication ignored final lease loss", err)
	}
	var state string
	var failed, issues int64
	if err := f.db.QueryRow(`SELECT (SELECT state FROM sources WHERE id=?),(SELECT failed FROM jobs WHERE id=?),(SELECT COUNT(*) FROM inventory_issues)`, f.source.ID, job.ID).Scan(&state, &failed, &issues); err != nil || state != "unknown" || failed != 0 || issues != 0 {
		t.Fatal("late fence failure partially committed", state, failed, issues, err)
	}
}

func TestScanFailureClassificationUsesSafeStableCodes(t *testing.T) {
	cases := []struct {
		cause          error
		inventory, job string
		retry          bool
	}{
		{context.DeadlineExceeded, "IO_ERROR", "JOB_TIMEOUT", true},
		{ErrMinimumFiles, "MINIMUM_FILES", "PROCESSING_FAILED", false},
		{ErrLimit, "SCAN_LIMIT", "PROCESSING_FAILED", false},
		{ErrCycle, "SYMLINK_CYCLE", "PROCESSING_FAILED", false},
		{ErrUnsafeEntry, "UNSAFE_ENTRY", "INVALID_MEDIA", false},
		{ErrChanged, "SOURCE_CHANGED", "SOURCE_CHANGED", true},
		{media.ErrContentChanged, "SOURCE_CHANGED", "SOURCE_CHANGED", true},
		{library.ErrSourceChanged, "SOURCE_CHANGED", "SOURCE_CHANGED", true},
		{ErrGeneration, "SOURCE_CHANGED", "SOURCE_CHANGED", true},
		{&os.PathError{Op: "open", Path: filepath.Join(t.TempDir(), "private-name"), Err: os.ErrNotExist}, "SOURCE_UNAVAILABLE", "SOURCE_UNAVAILABLE", true},
		{os.ErrPermission, "SOURCE_UNAVAILABLE", "SOURCE_UNAVAILABLE", true},
		{media.ErrSourceUnavailable, "SOURCE_UNAVAILABLE", "SOURCE_UNAVAILABLE", true},
		{errors.New("private decoder or filesystem failure"), "IO_ERROR", "IO_ERROR", true},
	}
	for _, c := range cases {
		code, job, retry := scanFailure(c.cause)
		if code != c.inventory || job != c.job || retry != c.retry {
			t.Fatal(c.cause, code, job, retry)
		}
	}
}
