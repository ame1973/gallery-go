//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
)

func artifactTempsFixture(t *testing.T, terminal bool) stillInputFixture {
	t.Helper()
	f := newStillInputFixture(t)
	f.svc.cacheRoot = t.TempDir()
	if err := os.Chmod(f.svc.cacheRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if terminal {
		if _, err := f.queue.Progress(t.Context(), f.lease, jobs.Counts{}, true); err != nil {
			t.Fatal(err)
		}
		if err := f.queue.Complete(t.Context(), f.lease); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func enqueueArtifactTemps(t *testing.T, f stillInputFixture, request artifactTempRequest) jobs.Job {
	t.Helper()
	input, err := f.svc.artifactTempInput(request)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	job, _, err := f.queue.EnqueueTx(t.Context(), tx, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return job
}

func claimArtifactTemps(t *testing.T, f stillInputFixture, request artifactTempRequest) jobs.Lease {
	t.Helper()
	job := enqueueArtifactTemps(t, f, request)
	lease, err := f.queue.Claim(t.Context(), "private-temp-maintenance", []string{"reconcile_temps"})
	if err != nil || lease.Job.ID != job.ID {
		t.Fatal("private temporary cleanup was not claimed", lease.Job, err)
	}
	return lease
}

func writeArtifactTemp(t *testing.T, f stillInputFixture, owner, name string, age time.Duration) string {
	t.Helper()
	if name == "" {
		name = store.NewID() + ".tmp"
	}
	directory := filepath.Join(f.svc.cacheRoot, "tmp", owner)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(directory, name)
	if err := os.WriteFile(file, []byte("private interrupted output"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	return file
}

func artifactBusinessCounts(t *testing.T, db *sql.DB) [6]int64 {
	t.Helper()
	var counts [6]int64
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM media),(SELECT COUNT(*) FROM media_revisions),(SELECT COUNT(*) FROM media_variants),(SELECT COUNT(*) FROM source_entries),(SELECT version FROM media WHERE id='input-media'),(SELECT version FROM source_entries WHERE id='input-entry')`).Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestArtifactTempsRemovesOnlyOldOwnedPrivateUnreferencedFiles(t *testing.T) {
	f := artifactTempsFixture(t, true)
	owner := f.lease.Job.ID
	old := writeArtifactTemp(t, f, owner, "", 25*time.Hour)
	retained := []string{
		writeArtifactTemp(t, f, owner, "", time.Hour),
		writeArtifactTemp(t, f, owner, "", -time.Hour),
		writeArtifactTemp(t, f, owner, "unknown.tmp", 25*time.Hour),
	}
	permissions := writeArtifactTemp(t, f, owner, "", 25*time.Hour)
	if err := os.Chmod(permissions, 0o644); err != nil {
		t.Fatal(err)
	}
	retained = append(retained, permissions)
	large := writeArtifactTemp(t, f, owner, "", 25*time.Hour)
	if err := os.Truncate(large, (8<<20)+1); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(large, past, past); err != nil {
		t.Fatal(err)
	}
	retained = append(retained, large)
	linked := writeArtifactTemp(t, f, owner, "", 25*time.Hour)
	outside := filepath.Join(t.TempDir(), "owned-link")
	if err := os.Link(linked, outside); err != nil {
		t.Fatal(err)
	}
	retained = append(retained, linked)
	symlink := filepath.Join(filepath.Dir(old), store.NewID()+".tmp")
	if err := os.Symlink(f.file, symlink); err != nil {
		t.Fatal(err)
	}
	retained = append(retained, symlink)
	directory := filepath.Join(filepath.Dir(old), store.NewID()+".tmp")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	retained = append(retained, directory)
	fifo := filepath.Join(filepath.Dir(old), store.NewID()+".tmp")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	retained = append(retained, fifo)
	referenced := writeArtifactTemp(t, f, owner, "", 25*time.Hour)
	relative := "tmp/" + owner + "/" + filepath.Base(referenced)
	if _, err := f.db.Exec(`INSERT INTO media_variants(id,revision_id,preset_key,preset_version,storage_path,mime,size_bytes,sha256,status,created_at) VALUES('retained-temp','input-revision','fixture_reference',1,?,'image/webp',1,?,'retired',0)`, relative, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	retained = append(retained, referenced)
	unknownJob := writeArtifactTemp(t, f, "unknown-owner", "", 25*time.Hour)
	final := filepath.Join(f.svc.cacheRoot, "variants", "unknown-final.webp")
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("private immutable bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := artifactBusinessCounts(t, f.db)
	request := artifactTempRequest{JobLimit: 100, EntryLimit: 1000}
	lease := claimArtifactTemps(t, f, request)
	report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request)
	if err != nil || report.scopes != 1 || report.unlinked != 1 || report.retained != int64(len(retained)) || report.entries != int64(len(retained)+1) || report.hasMore || report.limited || report.nextJobID != owner {
		t.Fatal("bounded temp result is not truthful", report, err)
	}
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old owned temp was not removed", err)
	}
	for _, name := range append(retained, unknownJob, final, outside, filepath.Dir(old), filepath.Join(f.svc.cacheRoot, artifactGateName)) {
		if _, err := os.Lstat(name); err != nil {
			t.Fatal("cleanup removed a retained file/shared directory/gate", name, err)
		}
	}
	if artifactBusinessCounts(t, f.db) != before {
		t.Fatal("temporary maintenance changed indexed metadata or READY rows")
	}
	if actual, err := os.ReadFile(f.file); err != nil || !bytes.Equal(actual, f.data) {
		t.Fatal("temporary maintenance touched source bytes", err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "succeeded" || job.Total != 1 || job.Done != 1 || job.Succeeded != 1 || job.Failed != 0 {
		t.Fatal("scope work units were reported as file/full-cache units", job, err)
	}
	encoded, _ := json.Marshal(report)
	if string(encoded) != "{}" || strings.Contains(fmt.Sprint(report), f.svc.cacheRoot) {
		t.Fatal("private maintenance report exposed paths", string(encoded))
	}
	second := claimArtifactTemps(t, f, request)
	replay, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, second, request)
	if err != nil || replay.unlinked != 0 || replay.scopes != 2 {
		t.Fatal("absent files were recounted as successful removals", replay, err)
	}
	repeated, err := f.queue.Get(t.Context(), second.Job.ID)
	if err != nil || repeated.Total != 2 || repeated.Skipped != 2 || repeated.Succeeded != 0 {
		t.Fatal("retry did not preserve scope/file distinction", repeated, err)
	}
}

func TestArtifactTempsRejectsWrongWorkIdentityLeaseAndConsumedBudgetBeforeIO(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, stillInputFixture, jobs.Lease){
		"wrong kind": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			_, err := f.db.Exec(`UPDATE jobs SET kind='scan' WHERE id=?`, lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"wrong resource": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			_, err := f.db.Exec(`UPDATE jobs SET resource_key='unrelated' WHERE id=?`, lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"wrong historical dedupe": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			_, err := f.db.Exec(`UPDATE jobs SET dedupe_token='unrelated' WHERE id=?`, lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"wrong payload": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			_, err := f.db.Exec(`UPDATE jobs SET payload_json='{"version":2}' WHERE id=?`, lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"missing dependency": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			_, err := f.db.Exec(`DELETE FROM job_resources WHERE job_id=?`, lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"expired": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			_, err := f.db.Exec(`UPDATE jobs SET lease_expires_at=0 WHERE id=?`, lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"reclaimed": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			_, err := f.db.Exec(`UPDATE jobs SET lease_owner='successor' WHERE id=?`, lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"prior attempt": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			_, err := f.db.Exec(`UPDATE jobs SET attempts=attempts+1 WHERE id=?`, lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"cancelled": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			_, err := f.db.Exec(`UPDATE jobs SET cancel_requested=1 WHERE id=?`, lease.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
		},
		"source unavailable": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			if _, err := f.db.Exec(`UPDATE sources SET state='unavailable' WHERE id='input-source'`); err != nil {
				t.Fatal(err)
			}
		},
		"already reserved": func(t *testing.T, f stillInputFixture, lease jobs.Lease) {
			if _, err := f.queue.Progress(t.Context(), lease, jobs.Counts{}, true); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			file := writeArtifactTemp(t, f, f.lease.Job.ID, "", 25*time.Hour)
			request := artifactTempRequest{JobLimit: 100, EntryLimit: 1000}
			lease := claimArtifactTemps(t, f, request)
			mutate(t, f, lease)
			before := inputMutationCounts(t, f.db)
			if _, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request); err == nil {
				t.Fatal("invalid work consumed files or reserved another budget")
			}
			if _, err := os.Stat(file); err != nil || inputMutationCounts(t, f.db) != before {
				t.Fatal("rejected maintenance mutated files/business/job budget", err)
			}
			if _, err := os.Stat(filepath.Join(f.svc.cacheRoot, artifactGateName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected authority created a cache lock", err)
			}
		})
	}
}

func TestArtifactTempsRetainsLiveUnknownAndChangedOwnerScopes(t *testing.T) {
	for name, mutate := range map[string]string{
		"live":                  "",
		"unknown kind":          `UPDATE jobs SET kind='scan' WHERE kind='variant_still'`,
		"wrong owner payload":   `UPDATE jobs SET payload_json='{"version":2}' WHERE kind='variant_still'`,
		"missing owner binding": `DELETE FROM job_resources WHERE resource_key='source:input-source'`,
		"case-folded binding":   `UPDATE job_resources SET resource_key=upper(resource_key) WHERE resource_key='source:input-source'`,
		"empty binding":         `UPDATE job_resources SET resource_key='source:' WHERE resource_key='source:input-source'`,
		"wrong owner identity":  `UPDATE jobs SET resource_key='unrelated' WHERE kind='variant_still'`,
	} {
		t.Run(name, func(t *testing.T) {
			f := artifactTempsFixture(t, name != "live")
			file := writeArtifactTemp(t, f, f.lease.Job.ID, "", 25*time.Hour)
			if mutate != "" {
				if _, err := f.db.Exec(mutate); err != nil {
					t.Fatal(err)
				}
			}
			request := artifactTempRequest{JobLimit: 100, EntryLimit: 1000}
			lease := claimArtifactTemps(t, f, request)
			report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request)
			if err != nil || report.unlinked != 0 || report.entries != 0 {
				t.Fatal("unknown/live owner was enumerated or removed", report, err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal(err)
			}
			job, err := f.queue.Get(t.Context(), lease.Job.ID)
			if err != nil || job.Skipped != 1 || job.Total != 1 || job.Status != "succeeded" {
				t.Fatal("retained scope was not a skipped work unit", job, err)
			}
		})
	}
}

func TestArtifactTempsBoundsNamesPagesEntriesAndReportsLimitsAsFailure(t *testing.T) {
	f := artifactTempsFixture(t, true)
	for _, request := range []artifactTempRequest{
		{JobLimit: 0, EntryLimit: 1}, {JobLimit: 101, EntryLimit: 1},
		{JobLimit: 1, EntryLimit: 0}, {JobLimit: 1, EntryLimit: 1001},
		{AfterJobID: "../escape", JobLimit: 1, EntryLimit: 1},
		{AfterJobID: "bad\x00", JobLimit: 1, EntryLimit: 1},
		{AfterJobID: strings.Repeat("x", 129), JobLimit: 1, EntryLimit: 1},
	} {
		if _, err := f.svc.artifactTempInput(request); !errors.Is(err, errArtifactTemps) {
			t.Fatal("invalid explicit maintenance bound accepted", request, err)
		}
	}
	for _, name := range []string{"../escape.tmp", "UPPER.tmp", "00000000-0000-4000-8000-000000000000.tmp", store.NewID() + ".tmp/escape"} {
		if artifactTempName.MatchString(name) {
			t.Fatal("unowned name accepted", name)
		}
	}
	files := []string{writeArtifactTemp(t, f, f.lease.Job.ID, "", 25*time.Hour), writeArtifactTemp(t, f, f.lease.Job.ID, "", 25*time.Hour)}
	request := artifactTempRequest{JobLimit: 1, EntryLimit: 1}
	lease := claimArtifactTemps(t, f, request)
	report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request)
	if err != nil || !report.limited || !report.hasMore || report.nextJobID != "" || report.entries != 1 || report.unlinked != 1 {
		t.Fatal("bounded enumeration claimed a complete sweep", report, err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "failed" || job.Failed != 1 || job.Succeeded != 0 || job.Done != 1 {
		t.Fatal("entry budget was treated as successful cleanup", job, err)
	}
	remaining := 0
	for _, name := range files {
		if _, err := os.Stat(name); err == nil {
			remaining++
		}
	}
	if remaining != 1 {
		t.Fatal("entry budget was exceeded", remaining)
	}
	page := claimArtifactTemps(t, f, artifactTempRequest{AfterJobID: f.lease.Job.ID, JobLimit: 1, EntryLimit: 10})
	pageReport, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, page, artifactTempRequest{AfterJobID: f.lease.Job.ID, JobLimit: 1, EntryLimit: 10})
	if err != nil || pageReport.scopes != 1 || pageReport.unlinked != 0 || pageReport.nextJobID != lease.Job.ID {
		t.Fatal("private job-ID continuation changed scope", pageReport, err)
	}
}

func TestArtifactTempsTerminalStatesAreEligibleWithoutPurgingSourceOrSharedDirectories(t *testing.T) {
	for _, status := range []string{"succeeded", "failed", "cancelled", "dead"} {
		t.Run(status, func(t *testing.T) {
			f := artifactTempsFixture(t, false)
			switch status {
			case "succeeded":
				if _, err := f.queue.Progress(t.Context(), f.lease, jobs.Counts{}, true); err != nil {
					t.Fatal(err)
				}
				if err := f.queue.Complete(t.Context(), f.lease); err != nil {
					t.Fatal(err)
				}
			case "failed":
				if err := f.queue.Fail(t.Context(), f.lease, "PROCESSING_FAILED", false); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				if _, err := f.db.Exec(`UPDATE jobs SET cancel_requested=1 WHERE id=?`, f.lease.Job.ID); err != nil {
					t.Fatal(err)
				}
				if err := f.queue.AcknowledgeCancel(t.Context(), f.lease); err != nil {
					t.Fatal(err)
				}
			case "dead":
				if _, err := f.db.Exec(`UPDATE jobs SET max_attempts=1 WHERE id=?`, f.lease.Job.ID); err != nil {
					t.Fatal(err)
				}
				if err := f.queue.Fail(t.Context(), f.lease, "PROCESSING_FAILED", true); err != nil {
					t.Fatal(err)
				}
			}
			owner, err := f.queue.Get(t.Context(), f.lease.Job.ID)
			if err != nil || owner.Status != status {
				t.Fatal(owner, err)
			}
			file := writeArtifactTemp(t, f, owner.ID, "", 25*time.Hour)
			request := artifactTempRequest{JobLimit: 100, EntryLimit: 1000}
			lease := claimArtifactTemps(t, f, request)
			report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request)
			if err != nil || report.unlinked != 1 {
				t.Fatal(report, err)
			}
			if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Dir(file)); err != nil {
				t.Fatal("shared job directory was removed", err)
			}
			current, err := f.queue.Get(t.Context(), owner.ID)
			if err != nil || current.Version != owner.Version || current.Status != status {
				t.Fatal("cleanup rewrote owner history", current, err)
			}
		})
	}
}

func TestArtifactTempsUnsafeParentDirectoriesAreRetainedWithoutTraversal(t *testing.T) {
	for _, location := range []string{"tmp-link", "job-link", "tmp-permissions", "job-permissions"} {
		t.Run(location, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			file := writeArtifactTemp(t, f, f.lease.Job.ID, "", 25*time.Hour)
			parent := filepath.Dir(file)
			if strings.HasPrefix(location, "tmp-") {
				parent = filepath.Dir(parent)
			}
			if strings.HasSuffix(location, "link") {
				if err := os.Rename(parent, parent+"-owned"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(parent+"-owned", parent); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			request := artifactTempRequest{JobLimit: 100, EntryLimit: 1000}
			lease := claimArtifactTemps(t, f, request)
			report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request)
			if err != nil || report.unlinked != 0 || report.entries != 0 {
				t.Fatal(report, err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("unsafe parent was followed for removal", err)
			}
			if data, err := os.ReadFile(f.file); err != nil || !bytes.Equal(data, f.data) {
				t.Fatal("unsafe parent changed source", err)
			}
		})
	}
}

func TestArtifactTempsRechecksOwnerAndLeaseAfterBudgetAndHeartbeat(t *testing.T) {
	for _, name := range []string{"owner-requeued", "source-warning", "lease-after-heartbeat", "lease-after-scope-audit"} {
		t.Run(name, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			file := writeArtifactTemp(t, f, f.lease.Job.ID, "", 25*time.Hour)
			request := artifactTempRequest{JobLimit: 100, EntryLimit: 1000}
			lease := claimArtifactTemps(t, f, request)
			var query string
			switch name {
			case "owner-requeued":
				query = fmt.Sprintf(`CREATE TRIGGER change_temp_owner AFTER UPDATE OF total_known ON jobs WHEN NEW.id='%s' AND OLD.total_known=0 AND NEW.total_known=1 BEGIN UPDATE jobs SET status='queued',version=version+1 WHERE id='%s'; END`, lease.Job.ID, f.lease.Job.ID)
			case "source-warning":
				query = fmt.Sprintf(`CREATE TRIGGER change_temp_source AFTER UPDATE OF total_known ON jobs WHEN NEW.id='%s' AND OLD.total_known=0 AND NEW.total_known=1 BEGIN UPDATE sources SET state='unavailable'; END`, lease.Job.ID)
			case "lease-after-heartbeat":
				query = fmt.Sprintf(`CREATE TABLE temp_test_heartbeat(n INTEGER); INSERT INTO temp_test_heartbeat VALUES(0); CREATE TRIGGER expire_temp_heartbeat AFTER UPDATE OF heartbeat_at ON jobs WHEN NEW.id='%s' BEGIN UPDATE temp_test_heartbeat SET n=n+1; UPDATE jobs SET lease_expires_at=0 WHERE id=NEW.id AND (SELECT n FROM temp_test_heartbeat)=2; END`, lease.Job.ID)
			case "lease-after-scope-audit":
				query = fmt.Sprintf(`CREATE TRIGGER expire_temp_audit AFTER INSERT ON audit_events WHEN NEW.event_type='artifact_temp_scope_checked' BEGIN UPDATE jobs SET lease_expires_at=0 WHERE id='%s'; END`, lease.Job.ID)
			}
			if _, err := f.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request)
			if name == "owner-requeued" {
				if err != nil || report.unlinked != 0 || report.retained != 1 {
					t.Fatal("late owner state was not retained", report, err)
				}
			} else if err == nil {
				t.Fatal("late lease/source fence was accepted", report)
			}
			_, statErr := os.Stat(file)
			if name == "lease-after-scope-audit" {
				if !errors.Is(statErr, os.ErrNotExist) || report.unlinked != 1 {
					t.Fatal("fixture did not reach post-unlink fence", report, statErr)
				}
				job, err := f.queue.Get(t.Context(), lease.Job.ID)
				if err != nil || job.Done != 0 || job.Succeeded != 0 || job.Total != 1 {
					t.Fatal("late audit/lease progress escaped rollback", job, err)
				}
			} else if statErr != nil {
				t.Fatal("pre-unlink rejection removed a temp", statErr)
			}
		})
	}
}

func TestArtifactTempsIdentityBindsCacheBoundsAndZeroUnitBudgetOnce(t *testing.T) {
	f := artifactTempsFixture(t, true)
	request := artifactTempRequest{AfterJobID: "zzzz", JobLimit: 1, EntryLimit: 1}
	input, err := f.svc.artifactTempInput(request)
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := f.svc.artifactTempInput(request)
	if err != nil || input.ResourceKey != repeat.ResourceKey || !bytes.Equal(input.Payload, repeat.Payload) {
		t.Fatal("private work identity is not deterministic", err)
	}
	changed := request
	changed.EntryLimit++
	other, err := f.svc.artifactTempInput(changed)
	if err != nil || input.ResourceKey == other.ResourceKey {
		t.Fatal("changed bounds coalesced", err)
	}
	otherCache, err := New(nil, nil, t.TempDir(), nil).artifactTempInput(request)
	if err != nil || otherCache.ResourceKey == input.ResourceKey {
		t.Fatal("cache configuration was not bound", err)
	}
	lease := claimArtifactTemps(t, f, request)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_empty_temp_finish BEFORE INSERT ON audit_events WHEN NEW.event_type='artifact_temps_reconciled' BEGIN SELECT RAISE(ABORT,'injected empty completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request)
	if err == nil || report.scopes != 0 || report.unlinked != 0 {
		t.Fatal("empty budget fixture did not fail at completion", report, err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || !job.TotalKnown || job.Total != 0 || job.Done != 0 {
		t.Fatal("zero-unit attempt did not reserve its budget", job, err)
	}
	if _, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request); !errors.Is(err, jobs.ErrState) {
		t.Fatal("zero-unit same-attempt replay was accepted", err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER reject_empty_temp_finish`); err != nil {
		t.Fatal(err)
	}
	if err := f.queue.Fail(t.Context(), lease, "PROCESSING_FAILED", false); err != nil {
		t.Fatal(err)
	}
	next := claimArtifactTemps(t, f, request)
	if report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, next, request); err != nil || report.scopes != 0 || report.hasMore {
		t.Fatal("new empty batch did not complete truthfully", report, err)
	}
}

func TestArtifactTempsLateJournalFailureNeverClaimsUncommittedDeletionSuccess(t *testing.T) {
	f := artifactTempsFixture(t, true)
	file := writeArtifactTemp(t, f, f.lease.Job.ID, "", 25*time.Hour)
	request := artifactTempRequest{JobLimit: 100, EntryLimit: 1000}
	lease := claimArtifactTemps(t, f, request)
	if _, err := f.db.Exec(`CREATE TRIGGER reject_temp_audit BEFORE INSERT ON audit_events WHEN NEW.event_type='artifact_temp_scope_checked' BEGIN SELECT RAISE(ABORT,'injected private audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request)
	if err == nil || report.unlinked != 1 || report.scopes != 0 {
		t.Fatal("late SQL failure did not retain the physical/committed distinction", report, err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("fixture did not reach unlink-before-journal", err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || !job.TotalKnown || job.Total != 1 || job.Done != 0 || job.Succeeded != 0 {
		t.Fatal("failed journal committed successful work", job, err)
	}
	if _, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request); !errors.Is(err, jobs.ErrState) {
		t.Fatal("same-attempt reentry expanded/reapplied its budget", err)
	}
	var completed, scopes, orphan int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM audit_events WHERE event_type='artifact_temps_reconciled'),(SELECT COUNT(*) FROM audit_events WHERE event_type='artifact_temp_scope_checked'),(SELECT COUNT(*) FROM change_log c WHERE NOT EXISTS(SELECT 1 FROM outbox_events o WHERE o.change_seq=c.seq))`).Scan(&completed, &scopes, &orphan); err != nil || completed != 0 || scopes != 0 || orphan != 0 {
		t.Fatal("late failure left partial journal/outbox", completed, scopes, orphan, err)
	}
}

func TestArtifactTempsGateWaitLeavesSQLiteWriterAndBudgetAvailable(t *testing.T) {
	f := artifactTempsFixture(t, true)
	file := writeArtifactTemp(t, f, f.lease.Job.ID, "", 25*time.Hour)
	request := artifactTempRequest{JobLimit: 100, EntryLimit: 1000}
	lease := claimArtifactTemps(t, f, request)
	shared, err := f.svc.lockArtifacts(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.svc.reconcileArtifactTemps(ctx, f.queue, lease, request); done <- err }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, entry := range entries {
			target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if target == filepath.Join(f.svc.cacheRoot, artifactGateName) {
				count++
			}
		}
		if count >= 2 {
			break
		}
		select {
		case err := <-done:
			t.Fatal("cleanup did not wait", err)
		case <-deadline.C:
			t.Fatal("cleanup lock was not opened")
		case <-tick.C:
		}
	}
	writerCtx, writerCancel := context.WithTimeout(t.Context(), time.Second)
	defer writerCancel()
	tx, err := f.db.BeginTx(writerCtx, nil)
	if err != nil {
		t.Fatal("cleanup waited while holding SQLite writer", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-deadline.C:
		t.Fatal("cleanup did not cancel")
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.TotalKnown || job.Total != 0 {
		t.Fatal("waiting consumed a budget", job, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	if report, err := f.svc.reconcileArtifactTemps(t.Context(), f.queue, lease, request); err != nil || report.unlinked != 1 {
		t.Fatal(report, err)
	}
}
