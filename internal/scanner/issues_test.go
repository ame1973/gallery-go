package scanner

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func TestScanIssuesCoCommitWithMetadataAndSurviveImmutableReplay(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	bad := []byte("not a valid JPEG")
	badPath := writeInput(t, root, "Album/a-bad.jpg", bad)
	writeInput(t, root, "Album/b-good.png", metadataPNG(t))
	writeInput(t, root, "Album/c-unsupported.tiff", []byte("II+\x00\x08\x00\x00\x00"))
	writeInput(t, root, "notes.txt", []byte("ordinary non-media"))
	lease, generation := f.ready(t)
	before, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_job_issue BEFORE INSERT ON job_issues BEGIN SELECT RAISE(ABORT,'injected projection rejection'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, media.DefaultInspectPolicy()); err == nil {
		t.Fatal("metadata committed without durable issue history")
	}
	var issues, staged int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM job_issues),(SELECT COUNT(*) FROM inventory_media WHERE generation_id=?)`, generation.ID).Scan(&issues, &staged); err != nil || issues != 0 || staged != 0 {
		t.Fatal("diagnostic projection failure left partial metadata", issues, staged, err)
	}
	after, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || after.Done != before.Done || after.Failed != before.Failed {
		t.Fatal("rolled-back diagnostic consumed progress", before.Counts, after.Counts, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_job_issue`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, media.DefaultInspectPolicy()); err != nil {
		t.Fatal(err)
	}
	admin := jobs.NewAdmin(f.queue, f.auth)
	items, err := admin.Issues(t.Context(), f.owner, lease.Job.ID, "", 101)
	if err != nil || len(items) != 2 {
		t.Fatal("per-entry failures were not exposed to the current administrator", items, err)
	}
	snapshots := map[string]jobs.Issue{}
	for _, item := range items {
		if item.Attempt != lease.Job.Attempts || item.SourceID != f.source.ID || item.SourceConfigVersion != f.source.ConfigVersion || item.GenerationID == nil || *item.GenerationID != generation.ID || item.RelativePath == nil || item.Message == "" {
			t.Fatal("issue lost its pinned historical input context", item)
		}
		snapshots[*item.RelativePath] = item
	}
	if snapshots["Album/a-bad.jpg"].ErrorCode != "INVALID_MEDIA" || snapshots["Album/c-unsupported.tiff"].ErrorCode != "CODEC_UNAVAILABLE" {
		t.Fatal("safe metadata failure codes were replaced by raw or aggregate detail", snapshots)
	}
	if _, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, media.DefaultInspectPolicy()); err != nil {
		t.Fatal(err)
	}
	repeated, err := admin.Issues(t.Context(), f.owner, lease.Job.ID, "", 101)
	if err != nil || len(repeated) != 2 {
		t.Fatal("immutable replay duplicated issue rows", repeated, err)
	}
	for _, item := range repeated {
		if item.RelativePath == nil || snapshots[*item.RelativePath].ID != item.ID {
			t.Fatal("replay replaced historical issue identity", item)
		}
	}
	if _, err := f.inventory.Publish(t.Context(), lease, generation.ID); err != nil {
		t.Fatal(err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "failed" || job.Total != 12 || job.Done != 12 || job.Failed != 2 || job.Skipped != 1 || job.Succeeded != 9 {
		t.Fatal("issue count was confused with cumulative work counts", job, job.Counts, err)
	}
	if actual, err := os.ReadFile(badPath); err != nil || !bytes.Equal(actual, bad) {
		t.Fatal("diagnostics rewrote source input", err)
	}
}

func TestScanIssueHistoryIncludesGenerationlessFailuresAndSurvivesSourceDeletion(t *testing.T) {
	for _, generationless := range []bool{false, true} {
		t.Run(map[bool]string{false: "mount-loss", true: "unconfigured-worker"}[generationless], func(t *testing.T) {
			f := inventorySetup(t)
			if generationless {
				f.inventory.sources = library.NewSources(f.db, f.auth, f.queue, nil)
			} else if err := os.Remove(filepath.Join(f.rootPath, ".mounted")); err != nil {
				t.Fatal(err)
			}
			scans := NewScans(f.db, f.auth, f.queue)
			job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{SourceID: &f.source.ID, Mode: "full"}, store.NewID())
			if err != nil {
				t.Fatal(err)
			}
			startScanExecutor(t, f, scans)
			finished := waitScanResult(t, f, job.ID)
			if finished.Status != "dead" || finished.Attempts != 3 || finished.Failed != 3 {
				t.Fatal("source failure attempts were not retained", finished, finished.Counts)
			}
			admin := jobs.NewAdmin(f.queue, f.auth)
			items, err := admin.Issues(t.Context(), f.owner, job.ID, "", 101)
			if err != nil || len(items) != 3 {
				t.Fatal("source-level diagnostics disappeared", items, err)
			}
			for _, item := range items {
				if item.RelativePath == nil || *item.RelativePath != "." || item.SourceID != f.source.ID || item.SourceConfigVersion != f.source.ConfigVersion || (item.GenerationID == nil) != generationless {
					t.Fatal("source diagnostics guessed a generation/path/configuration", item)
				}
			}
			source, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.sources.DeleteEmpty(t.Context(), f.owner, source.ID, source.Version); err != nil {
				t.Fatal(err)
			}
			retained, err := admin.Issues(t.Context(), f.owner, job.ID, "", 101)
			if err != nil || len(retained) != 3 {
				t.Fatal("removing an unindexed registration erased job history", retained, err)
			}
			for index := range items {
				if items[index].ID != retained[index].ID {
					t.Fatal("source removal replaced immutable diagnostic identities")
				}
			}
		})
	}
}

func TestGenerationlessIssueAuditAndLeaseFailuresRollBackTogether(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{SourceID: &f.source.ID, Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.queue.Claim(t.Context(), "issue-worker", []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	target := scanTarget{sourceID: f.source.ID, config: f.source.ConfigVersion, scope: "."}
	if _, err := f.db.Exec(`CREATE TRIGGER reject_issue_audit BEFORE INSERT ON audit_events WHEN NEW.event_type='scan_target_failed' BEGIN SELECT RAISE(ABORT,'injected audit rejection'); END`); err != nil {
		t.Fatal(err)
	}
	if err := scans.recordSourceFailure(t.Context(), lease, f.inventory, target, Generation{}, "SOURCE_UNAVAILABLE", "SOURCE_UNAVAILABLE", media.ErrSourceUnavailable); err == nil {
		t.Fatal("generationless issue committed without audit")
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM job_issues`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed source transaction leaked an issue", count, err)
	}
	current, err := f.queue.Get(t.Context(), job.ID)
	if err != nil || current.Failed != 0 || current.Done != 0 {
		t.Fatal("rolled-back source issue consumed progress", current.Counts, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_issue_audit`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER expire_after_issue BEFORE INSERT ON audit_events WHEN NEW.event_type='scan_target_failed' BEGIN UPDATE jobs SET lease_expires_at=0 WHERE id=NEW.resource_id; END`); err != nil {
		t.Fatal(err)
	}
	if err := scans.recordSourceFailure(t.Context(), lease, f.inventory, target, Generation{}, "SOURCE_UNAVAILABLE", "SOURCE_UNAVAILABLE", media.ErrSourceUnavailable); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("late lease loss committed diagnostic history", err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM job_issues`).Scan(&count); err != nil || count != 0 {
		t.Fatal("late lease rollback leaked an issue", count, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER expire_after_issue`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := scans.recordSourceFailure(t.Context(), lease, f.inventory, target, Generation{}, "SOURCE_UNAVAILABLE", "SOURCE_UNAVAILABLE", media.ErrSourceUnavailable); err != nil {
			t.Fatal(err)
		}
	}
	current, err = f.queue.Get(t.Context(), job.ID)
	if err != nil || current.Failed != 1 || current.Done != 1 {
		t.Fatal("same source attempt diagnostic double-counted work", current.Counts, err)
	}
	if _, err := f.db.Exec(`UPDATE jobs SET lease_expires_at=0 WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := scans.recordSourceFailure(t.Context(), lease, f.inventory, target, Generation{}, "IO_ERROR", "IO_ERROR", errors.New("private path must not be projected")); !errors.Is(err, jobs.ErrLeaseLost) {
		t.Fatal("stale owner wrote diagnostic history", err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM job_issues`).Scan(&count); err != nil || count != 1 {
		t.Fatal("expired lease changed committed issue history", count, err)
	}
}
