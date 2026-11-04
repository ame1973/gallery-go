package scanner

import (
	"context"
	"encoding/json"
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

func startScanExecutor(t *testing.T, f inventoryFixture, scans *Scans) {
	t.Helper()
	handler, err := scans.Handler(f.inventory, media.DefaultInspectPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.queue.Run(ctx, "worker", map[string]jobs.Handler{"scan": handler}) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("scan executor shutdown", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("scan executor did not stop")
		}
	})
}

func waitScanResult(t *testing.T, f inventoryFixture, id string) jobs.Job {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		job, err := f.queue.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != "queued" && job.Status != "running" {
			return job
		}
		select {
		case <-timer.C:
			t.Fatal("timed out waiting for real scan execution")
		case <-tick.C:
		}
	}
}

func TestScanHandlerExecutesActualNativeInventoryAndOwnedPublication(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	data := publicationPNG(t, 12)
	name := writeInput(t, root, "Album/photo.png", data)
	scans := NewScans(f.db, f.auth, f.queue)
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{SourceID: &f.source.ID, Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	startScanExecutor(t, f, scans)
	result := waitScanResult(t, f, job.ID)
	if result.Status != "succeeded" || result.Attempts != 1 || result.Succeeded != 3 || result.Failed != 0 || result.Done != 3 || !result.TotalKnown {
		t.Fatal("scan executor invented or lost outcomes", result)
	}
	var count, width int64
	var state string
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media),(SELECT width FROM media),(SELECT state FROM inventory_generations WHERE job_id=?)`, job.ID).Scan(&count, &width, &state); err != nil || count != 1 || width != 12 || state != "published" {
		t.Fatal("native pipeline did not publish", count, width, state, err)
	}
	original, err := os.ReadFile(name)
	if err != nil || string(original) != string(data) {
		t.Fatal("executor changed read-only original", err)
	}
	current, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil || current.ScanGeneration != 1 || current.State != "healthy" {
		t.Fatal(current, err)
	}
}

func TestScanHandlerProcessesHealthySourcesDespiteAnotherSourceLoss(t *testing.T) {
	f := inventorySetup(t)
	firstRoot := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, firstRoot, "Album/old.png", publicationPNG(t, 12))
	first := f.publish(t, "full", ".")
	otherRoot := walkRoot(t)
	writeInput(t, otherRoot, "Album/new.png", publicationPNG(t, 13))
	f.sources = library.NewSources(f.db, f.auth, f.queue, map[string]media.SourceRoot{"fixture": firstRoot, "other": otherRoot})
	f.inventory.sources = f.sources
	settings := library.DefaultSourceSettings()
	settings.LibraryID = f.source.LibraryID
	settings.RootName = "other"
	other, err := f.sources.Create(t.Context(), f.owner, settings, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(firstRoot.Path, firstRoot.Marker)); err != nil {
		t.Fatal(err)
	}
	scans := NewScans(f.db, f.auth, f.queue)
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	startScanExecutor(t, f, scans)
	result := waitScanResult(t, f, job.ID)
	if result.Status != "dead" || result.Attempts != 3 || result.Failed != 3 || result.ErrorCode == nil || *result.ErrorCode != "SOURCE_UNAVAILABLE" {
		t.Fatal("source loss was hidden or retries unbounded", result)
	}
	var mediaCount int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM media`).Scan(&mediaCount); err != nil || mediaCount != 2 {
		t.Fatal("source loss purged old index or retries duplicated new media", mediaCount, err)
	}
	unavailable, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil || unavailable.State != "unavailable" || unavailable.ScanGeneration != first.Number || unavailable.LastErrorCode == nil || *unavailable.LastErrorCode != "SOURCE_UNAVAILABLE" {
		t.Fatal("failed source blessed/deleted old inventory", unavailable, err)
	}
	healthy, err := f.sources.Get(t.Context(), f.owner, other.ID)
	if err != nil || healthy.State != "healthy" || healthy.ScanGeneration < 1 {
		t.Fatal("healthy remaining source was skipped", healthy, err)
	}
	if _, err := os.Stat(filepath.Join(firstRoot.Path, firstRoot.Marker)); !os.IsNotExist(err) {
		t.Fatal("worker recreated missing source marker", err)
	}
}

func TestScanHandlerMinimumFailureDoesNotPublishOrRetryAsSuccess(t *testing.T) {
	f := inventorySetup(t)
	writeInput(t, media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}, "photo.png", publicationPNG(t, 12))
	minimum := int64(2)
	var err error
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{MinimumFiles: &minimum})
	if err != nil {
		t.Fatal(err)
	}
	scans := NewScans(f.db, f.auth, f.queue)
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	startScanExecutor(t, f, scans)
	result := waitScanResult(t, f, job.ID)
	if result.Status != "failed" || result.Attempts != 1 || result.Succeeded != 1 || result.Failed != 1 || !result.TotalKnown {
		t.Fatal("minimum guard became success or false retry work", result)
	}
	var count int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media)+(SELECT COUNT(*) FROM source_entries)+(SELECT scan_generation FROM sources)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("minimum failure mutated index", count, err)
	}
	current, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil || current.State != "error" || current.LastErrorCode == nil || *current.LastErrorCode != "MINIMUM_FILES" {
		t.Fatal("safe scan diagnostic missing", current, err)
	}
}

func TestScanHandlerRejectsUnversionedMalformedOrUnboundStoredIntents(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	handler, err := scans.Handler(f.inventory, media.DefaultInspectPolicy())
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`{}`, `{"version":1,"version":1,"mode":"full"}`, `{"version":1,"mode":"full","extra":0}`, `{"version":2,"mode":"full"}`, `{"version":1,"mode":null}`} {
		job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`UPDATE jobs SET payload_json=? WHERE id=?`, payload, job.ID); err != nil {
			t.Fatal(err)
		}
		lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
		if err != nil {
			t.Fatal(err)
		}
		var failure *jobs.Failure
		if err := handler(t.Context(), lease); !errors.As(err, &failure) || failure.Code != "UNSUPPORTED_JOB" || failure.Retryable {
			t.Fatal("malformed stored job gained scan authority", payload, err)
		}
		if err := f.queue.Fail(t.Context(), lease, "UNSUPPORTED_JOB", false); err != nil {
			t.Fatal(err)
		}
	}
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`DELETE FROM job_resources WHERE job_id=? AND resource_key=?`, job.ID, "source:"+f.source.ID); err != nil {
		t.Fatal(err)
	}
	lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	var failure *jobs.Failure
	if err := handler(t.Context(), lease); !errors.As(err, &failure) || failure.Code != "UNSUPPORTED_JOB" {
		t.Fatal("unbound private target was executed", err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inventory_generations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("invalid intent touched inventory", count, err)
	}
	if _, err := scans.Handler(nil, media.DefaultInspectPolicy()); !errors.Is(err, ErrPolicy) {
		t.Fatal("nil inventory accepted", err)
	}
	invalid := media.DefaultInspectPolicy()
	invalid.MaxPixels = 0
	if _, err := scans.Handler(f.inventory, invalid); !errors.Is(err, media.ErrInspectPolicy) {
		t.Fatal("invalid inspect limits accepted", err)
	}
}

func TestScanFailureHealthGenerationAndProgressRollBackTogether(t *testing.T) {
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
	if _, err := f.db.Exec(`CREATE TRIGGER fail_scan_health BEFORE INSERT ON audit_events WHEN NEW.event_type='source_scan_failed' BEGIN SELECT RAISE(ABORT,'injected final health audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), lease); err == nil {
		t.Fatal("failure reporting survived failed audit")
	}
	current, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil || current.State != "unknown" || current.LastErrorCode != nil {
		t.Fatal("health escaped transaction rollback", current, err)
	}
	result, err := f.queue.Get(t.Context(), job.ID)
	if err != nil || result.Failed != 0 || result.Done != 0 {
		t.Fatal("failed verification count escaped rollback", result, err)
	}
	var state string
	var count int
	if err := f.db.QueryRow(`SELECT state,(SELECT COUNT(*) FROM inventory_issues) FROM inventory_generations WHERE job_id=?`, job.ID).Scan(&state, &count); err != nil || state != "walking" || count != 0 {
		t.Fatal("generation failure escaped rollback", state, count, err)
	}
	var payload scanPayload
	if err := json.Unmarshal(result.Payload, &payload); err != nil || payload.Version != 1 {
		t.Fatal(err)
	}
}
