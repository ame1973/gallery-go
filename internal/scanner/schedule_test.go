package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func TestScheduleDuePinsInitialFullAndBoundsFailedFullDispatch(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	now := time.Now().UTC().Truncate(time.Millisecond)
	scans.now = func() time.Time { return now }
	if err := os.Remove(filepath.Join(f.rootPath, ".mounted")); err != nil {
		t.Fatal(err)
	}
	after, count, err := scans.ScheduleDue(t.Context(), f.sources, "")
	if err != nil || count != 1 || after != f.source.ID {
		t.Fatal(after, count, err)
	}
	var id, mode, scope string
	var actor *string
	var config, next, full int64
	if err := f.db.QueryRow(`SELECT j.id,json_extract(j.payload_json,'$.mode'),j.created_by_user_id,t.source_config_version,t.scope_path,s.next_scan_at,s.next_full_scan_at FROM jobs j JOIN scan_targets t ON t.job_id=j.id JOIN sources s ON s.id=t.source_id`).Scan(&id, &mode, &actor, &config, &scope, &next, &full); err != nil || mode != "full" || actor != nil || config != f.source.ConfigVersion || scope != "." || next != now.Add(60*time.Second).UnixMilli() || full != now.Add(60*time.Second).UnixMilli() {
		t.Fatal("scheduled intent/evidence incorrect", id, mode, actor, config, scope, next, full, err)
	}
	if _, count, err := scans.ScheduleDue(t.Context(), f.sources, ""); err != nil || count != 0 {
		t.Fatal("dispatch ignored source poll deadline", count, err)
	}
	now = now.Add(60 * time.Second)
	if _, count, err := scans.ScheduleDue(t.Context(), f.sources, ""); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var jobs, coalesced int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs),(SELECT COUNT(*) FROM audit_events WHERE event_type='scan_schedule_coalesced')`).Scan(&jobs, &coalesced); err != nil || jobs != 1 || coalesced != 1 {
		t.Fatal("active full intent was expanded or duplicated", jobs, coalesced, err)
	}
	lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.queue.Fail(t.Context(), lease, "SOURCE_UNAVAILABLE", false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(60 * time.Second)
	if _, count, err := scans.ScheduleDue(t.Context(), f.sources, ""); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='queued' AND json_extract(payload_json,'$.mode')='full'`).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatal("failed full work was silently downgraded", jobs, err)
	}
	var generation int64
	var lastFull *int64
	if err := f.db.QueryRow(`SELECT scan_generation,last_full_scan_at,next_full_scan_at FROM sources WHERE id=?`, f.source.ID).Scan(&generation, &lastFull, &full); err != nil || generation != 0 || lastFull != nil || full != now.Add(60*time.Second).UnixMilli() {
		t.Fatal("dispatch manufactured healthy full evidence", generation, lastFull, full, err)
	}
	if _, err := os.Stat(filepath.Join(f.rootPath, ".mounted")); !os.IsNotExist(err) {
		t.Fatal("scheduler created source marker", err)
	}
}

func TestScheduleDueChoosesIncrementalAndPeriodicFullAfterHealthyPublication(t *testing.T) {
	f := inventorySetup(t)
	writeInput(t, media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}, "photo.png", publicationPNG(t, 12))
	published := f.publish(t, "full", ".")
	scans := NewScans(f.db, f.auth, f.queue)
	var due, full int64
	if err := f.db.QueryRow(`SELECT next_scan_at,next_full_scan_at FROM sources WHERE id=?`, f.source.ID).Scan(&due, &full); err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(due)
	scans.now = func() time.Time { return now }
	if _, count, err := scans.ScheduleDue(t.Context(), f.sources, ""); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var mode string
	if err := f.db.QueryRow(`SELECT json_extract(payload_json,'$.mode') FROM jobs WHERE status='queued'`).Scan(&mode); err != nil || mode != "incremental" {
		t.Fatal(mode, err)
	}
	lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.queue.Fail(t.Context(), lease, "IO_ERROR", false); err != nil {
		t.Fatal(err)
	}
	now = time.UnixMilli(full)
	if _, err := f.db.Exec(`UPDATE sources SET next_scan_at=? WHERE id=?`, now.Add(time.Hour).UnixMilli(), f.source.ID); err != nil {
		t.Fatal(err)
	}
	if _, count, err := scans.ScheduleDue(t.Context(), f.sources, ""); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := f.db.QueryRow(`SELECT json_extract(payload_json,'$.mode') FROM jobs WHERE status='queued'`).Scan(&mode); err != nil || mode != "full" {
		t.Fatal(mode, err)
	}
	var generation int64
	if err := f.db.QueryRow(`SELECT scan_generation FROM sources WHERE id=?`, f.source.ID).Scan(&generation); err != nil || generation != published.Number {
		t.Fatal("scheduled dispatch published inventory", generation, err)
	}
}

func TestScheduleCoalescesManualIntentAndSourceEditCancelsItsDependency(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	manual, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{SourceID: &f.source.ID, Mode: "full"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, count, err := scans.ScheduleDue(t.Context(), f.sources, ""); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var jobs, resources int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs),(SELECT COUNT(*) FROM job_resources WHERE job_id=?)`, manual.ID).Scan(&jobs, &resources); err != nil || jobs != 1 || resources != 1 {
		t.Fatal("manual intent expanded", jobs, resources, err)
	}
	minimum := int64(1)
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{MinimumFiles: &minimum})
	if err != nil {
		t.Fatal(err)
	}
	old, err := f.queue.Get(t.Context(), manual.ID)
	if err != nil || old.Status != "cancelled" {
		t.Fatal("source edit did not cancel scheduled/manual coalesced job", old, err)
	}
	if _, count, err := scans.ScheduleDue(t.Context(), f.sources, ""); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	var config int64
	if err := f.db.QueryRow(`SELECT t.source_config_version FROM scan_targets t JOIN jobs j ON j.id=t.job_id WHERE j.status='queued'`).Scan(&config); err != nil || config != f.source.ConfigVersion {
		t.Fatal("new schedule kept stale config", config, err)
	}
}

func TestConcurrentSchedulersDispatchEachSourceOnce(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	counts := make(chan int, 2)
	failures := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			_, count, err := scans.ScheduleDue(t.Context(), f.sources, "")
			counts <- count
			failures <- err
		})
	}
	workers.Wait()
	close(counts)
	close(failures)
	total := 0
	for count := range counts {
		total += count
	}
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if total != 1 {
		t.Fatal("concurrent schedulers repeated dispatch", total)
	}
	var jobs, audits int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs),(SELECT COUNT(*) FROM audit_events WHERE event_type IN ('scan_scheduled','scan_schedule_coalesced'))`).Scan(&jobs, &audits); err != nil || jobs != 1 || audits != 1 {
		t.Fatal(jobs, audits, err)
	}
}

func TestScheduleLateAuditFailureRollsBackDeadlinesJobsAndTargets(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_schedule BEFORE INSERT ON audit_events WHEN NEW.event_type='scan_scheduled' BEGIN SELECT RAISE(ABORT,'injected schedule audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scans.ScheduleDue(t.Context(), f.sources, ""); err == nil {
		t.Fatal("schedule survived audit failure")
	}
	var count, due int64
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs)+(SELECT COUNT(*) FROM scan_targets)+(SELECT COUNT(*) FROM job_resources),next_scan_at FROM sources`).Scan(&count, &due); err != nil || count != 0 || due != 0 {
		t.Fatal("partial schedule escaped rollback", count, due, err)
	}
}

func TestSchedulePagesLargeDueSetsAndSkipsUnconfiguredOrInactiveSources(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	roots := map[string]media.SourceRoot{"fixture": {Path: f.rootPath, Marker: ".mounted"}}
	for index := range 201 {
		name := fmt.Sprintf("root-%03d", index)
		root := media.SourceRoot{Path: filepath.Join(t.TempDir(), "never-created"), Marker: ".marker"}
		roots[name] = root
		if _, err := f.db.Exec(`INSERT INTO sources(id,library_id,name,root_path,root_config_sha256,mapping_json,is_active,created_at) VALUES(?,?,?,?,?,'{}',1,0)`, fmt.Sprintf("scheduled-%03d", index), f.source.LibraryID, name, root.Path, root.Signature()); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"unconfigured", "inactive"} {
		if _, err := f.db.Exec(`INSERT INTO sources(id,library_id,name,root_path,mapping_json,is_active,created_at) VALUES(?,?,?,?,'{}',?,0)`, name, f.source.LibraryID, name, filepath.Join(t.TempDir(), "never-created"), name != "inactive"); err != nil {
			t.Fatal(err)
		}
	}
	sources := library.NewSources(f.db, f.auth, f.queue, roots)
	after := ""
	total := 0
	for page := 0; page < 5; page++ {
		next, count, err := scans.ScheduleDue(t.Context(), sources, after)
		if err != nil || count > 100 {
			t.Fatal(next, count, err)
		}
		total += count
		if next == "" {
			break
		}
		if next <= after {
			t.Fatal("schedule cursor did not advance", after, next)
		}
		after = next
	}
	if total != 202 {
		t.Fatal("bounded source pages omitted targets", total)
	}
	var jobs, targets, untouched int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs),(SELECT COUNT(*) FROM scan_targets),(SELECT COUNT(*) FROM sources WHERE id IN ('unconfigured','inactive') AND next_scan_at=0)`).Scan(&jobs, &targets, &untouched); err != nil || jobs != 202 || targets != 202 || untouched != 2 {
		t.Fatal(jobs, targets, untouched, err)
	}
}

func TestRunScheduleDisabledCancellationAndDatabaseFailure(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	policy := DefaultSchedulePolicy()
	policy.Enabled = false
	if err := scans.RunSchedule(t.Context(), f.sources, policy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	policy.Enabled = true
	if err := scans.RunSchedule(ctx, f.sources, policy); err != nil {
		t.Fatal(err)
	}
	if err := scans.RunSchedule(t.Context(), nil, policy); err == nil {
		t.Fatal("nil source service accepted")
	}
	policy.Interval = 0
	if err := scans.RunSchedule(t.Context(), f.sources, policy); err == nil {
		t.Fatal("unsafe schedule policy accepted")
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("disabled/cancelled scheduler created work", count, err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := scans.RunSchedule(t.Context(), f.sources, DefaultSchedulePolicy()); err == nil {
		t.Fatal("database failure hidden as idle success")
	}
}

func TestScheduledPayloadRemainsCanonicalAndBounded(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	if _, _, err := scans.ScheduleDue(t.Context(), f.sources, ""); err != nil {
		t.Fatal(err)
	}
	lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	var payload scanPayload
	if err := json.Unmarshal(lease.Job.Payload, &payload); err != nil || payload.Version != 1 || payload.Mode != "full" || len(lease.Job.Payload) > 64 {
		t.Fatal(payload, err)
	}
	mode, count, err := scans.scanJob(t.Context(), lease)
	if err != nil || mode != "full" || count != 1 {
		t.Fatal(mode, count, err)
	}
}
