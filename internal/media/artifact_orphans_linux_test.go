//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
)

func artifactOrphanRequestForTest(t *testing.T, f stillInputFixture) artifactOrphanRequest {
	t.Helper()
	request := artifactOrphanRequest{MediaID: "input-media", SourceID: "input-source", RevisionID: "input-revision", Preset: "thumb_320", EntryLimit: 1000, ByteLimit: 64 << 20}
	if err := f.db.QueryRow(`SELECT version,source_sha256,source_size,source_mtime_ns FROM media_revisions WHERE id='input-revision'`).Scan(&request.RevisionVersion, &request.SourceSHA256, &request.SourceSize, &request.SourceMTime); err != nil {
		t.Fatal(err)
	}
	return request
}

func enqueueArtifactOrphans(t *testing.T, f stillInputFixture, request artifactOrphanRequest) jobs.Job {
	t.Helper()
	input, err := f.svc.artifactOrphanInput(request)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	job, created, err := f.queue.EnqueueTx(t.Context(), tx, input)
	if err != nil || !created {
		t.Fatal("immutable cleanup fixture did not create its own work", err)
	}
	for _, resource := range []string{"media:" + request.MediaID, "revision:" + request.RevisionID} {
		if err := f.queue.BindResourceTx(t.Context(), tx, job, resource); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return job
}

func claimArtifactOrphans(t *testing.T, f stillInputFixture, request artifactOrphanRequest) jobs.Lease {
	t.Helper()
	job := enqueueArtifactOrphans(t, f, request)
	lease, err := f.queue.Claim(t.Context(), "private-immutable-maintenance", []string{"reconcile_artifact"})
	if err != nil || lease.Job.ID != job.ID {
		t.Fatal("immutable cleanup fixture did not claim its work", lease.Job, err)
	}
	return lease
}

func writeArtifactOrphan(t *testing.T, f stillInputFixture, request artifactOrphanRequest, data []byte) (stillArtifact, string) {
	t.Helper()
	if data == nil {
		data = webpFixture(t, "lossless")
	}
	artifact, err := validateStillArtifact(t.Context(), request.RevisionID, request.Preset, stillSource(3, 2, nil), data)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise actual private storage, closing its shared gate before cleanup.
	// This is container/storage evidence, never source-derived pixels or READY.
	if err := f.svc.storeAndCloseArtifactForTest(t.Context(), f.lease.Job.ID, artifact, data); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(f.svc.cacheRoot, filepath.FromSlash(artifact.storage))
	old := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(name, old, old); err != nil {
		t.Fatal(err)
	}
	return artifact, name
}

func TestArtifactOrphansReconcilesOneOwnedCohortWithoutPublishingOrTouchingSources(t *testing.T) {
	f := artifactTempsFixture(t, true)
	request := artifactOrphanRequestForTest(t, f)
	artifact, file := writeArtifactOrphan(t, f, request, nil)
	before := artifactBusinessCounts(t, f.db)
	lease := claimArtifactOrphans(t, f, request)
	report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
	if err != nil || report.entries != 1 || report.bytes != artifact.size || report.unlinked != 1 || report.retained != 0 || report.limited {
		t.Fatal("immutable cohort observations are not truthful", report, err)
	}
	if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old owned unreferenced artifact was not unlinked", err)
	}
	for _, retained := range []string{filepath.Dir(file), filepath.Join(f.svc.cacheRoot, "tmp", f.lease.Job.ID), filepath.Join(f.svc.cacheRoot, artifactGateName)} {
		if _, err := os.Stat(retained); err != nil {
			t.Fatal("cleanup removed a shared directory or permanent gate", err)
		}
	}
	if artifactBusinessCounts(t, f.db) != before {
		t.Fatal("private cleanup changed indexed metadata or variants")
	}
	if data, err := os.ReadFile(f.file); err != nil || !bytes.Equal(data, f.data) {
		t.Fatal("private cleanup changed source bytes", err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "succeeded" || job.Total != 1 || job.Done != 1 || job.Succeeded != 1 || job.Failed != 0 {
		t.Fatal("one cohort was not accounted as one scope", job, err)
	}
	encoded, _ := json.Marshal(report)
	if string(encoded) != "{}" || strings.Contains(fmt.Sprint(report), f.svc.cacheRoot) {
		t.Fatal("private immutable report exposed internal fields", string(encoded))
	}
	next := claimArtifactOrphans(t, f, request)
	repeated, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, next, request)
	if err != nil || repeated.entries != 0 || repeated.unlinked != 0 {
		t.Fatal("absent bytes were fabricated as another unlink", repeated, err)
	}
	job, err = f.queue.Get(t.Context(), next.Job.ID)
	if err != nil || job.Total != 1 || job.Skipped != 1 || job.Succeeded != 0 {
		t.Fatal("empty owned cohort was not a skipped scope", job, err)
	}
}

func TestArtifactOrphansIdentityBindsAllPinsCacheAndFiniteBounds(t *testing.T) {
	f := artifactTempsFixture(t, true)
	request := artifactOrphanRequestForTest(t, f)
	input, err := f.svc.artifactOrphanInput(request)
	if err != nil || input.Kind != "reconcile_artifact" || input.Dedupe != input.ResourceKey {
		t.Fatal(input, err)
	}
	repeated, err := f.svc.artifactOrphanInput(request)
	if err != nil || repeated.ResourceKey != input.ResourceKey || !bytes.Equal(input.Payload, repeated.Payload) {
		t.Fatal("immutable work identity is not deterministic", err)
	}
	for _, change := range []func(*artifactOrphanRequest){
		func(r *artifactOrphanRequest) { r.MediaID = "other-media" },
		func(r *artifactOrphanRequest) { r.SourceID = "other-source" },
		func(r *artifactOrphanRequest) { r.RevisionID = "other-revision" },
		func(r *artifactOrphanRequest) { r.RevisionVersion++ },
		func(r *artifactOrphanRequest) { r.SourceSHA256 = strings.Repeat("a", 64) },
		func(r *artifactOrphanRequest) { r.SourceSize++ },
		func(r *artifactOrphanRequest) { r.SourceMTime++ },
		func(r *artifactOrphanRequest) { r.Preset = "thumb_720" },
		func(r *artifactOrphanRequest) { r.EntryLimit-- },
		func(r *artifactOrphanRequest) { r.ByteLimit-- },
	} {
		changed := request
		change(&changed)
		other, err := f.svc.artifactOrphanInput(changed)
		if err != nil || other.ResourceKey == input.ResourceKey {
			t.Fatal("different cohort/pins/bounds coalesced", changed, err)
		}
	}
	other, err := New(nil, nil, t.TempDir(), nil).artifactOrphanInput(request)
	if err != nil || other.ResourceKey == input.ResourceKey {
		t.Fatal("cache identity was not bound", err)
	}
	for _, change := range []func(*artifactOrphanRequest){
		func(r *artifactOrphanRequest) { r.MediaID = "../escape" },
		func(r *artifactOrphanRequest) { r.SourceID = "" },
		func(r *artifactOrphanRequest) { r.SourceID = "source\x00" },
		func(r *artifactOrphanRequest) { r.RevisionID = "one/two" },
		func(r *artifactOrphanRequest) { r.RevisionVersion = 0 },
		func(r *artifactOrphanRequest) { r.SourceSHA256 = strings.Repeat("A", 64) },
		func(r *artifactOrphanRequest) { r.SourceSize = 0 },
		func(r *artifactOrphanRequest) { r.SourceSize = (1 << 40) + 1 },
		func(r *artifactOrphanRequest) { r.Preset = "poster_720" },
		func(r *artifactOrphanRequest) { r.EntryLimit = 0 },
		func(r *artifactOrphanRequest) { r.EntryLimit = 1001 },
		func(r *artifactOrphanRequest) { r.ByteLimit = 0 },
		func(r *artifactOrphanRequest) { r.ByteLimit = (64 << 20) + 1 },
	} {
		changed := request
		change(&changed)
		if _, err := f.svc.artifactOrphanInput(changed); !errors.Is(err, errArtifactTemps) {
			t.Fatal("invalid immutable work was accepted", changed, err)
		}
	}
	if _, err := New(nil, nil, "relative", nil).artifactOrphanInput(request); !errors.Is(err, errArtifactTemps) {
		t.Fatal("relative cache accepted", err)
	}
}

func TestArtifactOrphansPreservesEveryVariantReferenceIncludingHistoricalRows(t *testing.T) {
	for _, status := range []string{"ready", "failed", "retired"} {
		t.Run(status, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			request := artifactOrphanRequestForTest(t, f)
			artifact, file := writeArtifactOrphan(t, f, request, nil)
			if _, err := f.db.Exec(`UPDATE media SET current_revision_id=NULL WHERE id='input-media'`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`INSERT INTO media_variants(id,revision_id,preset_key,preset_version,storage_path,mime,size_bytes,sha256,status,created_at) VALUES('historical-reference','input-revision','reference_only',1,?,'image/webp',1,?,?,0)`, artifact.storage, strings.Repeat("f", 64), status); err != nil {
				t.Fatal(err)
			}
			lease := claimArtifactOrphans(t, f, request)
			report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
			if err != nil || report.unlinked != 0 || report.retained != 1 || report.bytes != 0 {
				t.Fatal("a referenced historical artifact was read/removed", report, err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("referenced bytes disappeared", err)
			}
		})
	}
}

func TestArtifactOrphansRequiresOwnedPinsAndCanonicalTerminalCreationEvidence(t *testing.T) {
	for name, query := range map[string]string{
		"revision version":           `UPDATE media_revisions SET version=2`,
		"revision SHA":               `UPDATE media_revisions SET source_sha256='different'`,
		"revision size":              `UPDATE media_revisions SET source_size=source_size+1`,
		"revision mtime":             `UPDATE media_revisions SET source_mtime_ns=source_mtime_ns+1`,
		"media kind":                 `UPDATE media SET kind='video'`,
		"media source":               `UPDATE media SET source_entry_id=NULL`,
		"historical kind":            `UPDATE jobs SET kind='scan' WHERE kind='variant_still'`,
		"historical payload":         `UPDATE jobs SET payload_json='{"version":2}' WHERE kind='variant_still'`,
		"historical resource":        `UPDATE jobs SET resource_key='unrelated' WHERE kind='variant_still'`,
		"historical token":           `UPDATE jobs SET dedupe_token='unrelated' WHERE kind='variant_still'`,
		"historical primary binding": `DELETE FROM job_resources WHERE resource_key GLOB 'variant_still:*'`,
		"historical media binding":   `DELETE FROM job_resources WHERE resource_key='media:input-media'`,
		"historical source binding":  `UPDATE job_resources SET resource_key='source:unrelated' WHERE resource_key='source:input-source'`,
		"case-folded source binding": `UPDATE job_resources SET resource_key='SOURCE:input-source' WHERE resource_key='source:input-source'`,
	} {
		t.Run(name, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			request := artifactOrphanRequestForTest(t, f)
			_, file := writeArtifactOrphan(t, f, request, nil)
			if _, err := f.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			lease := claimArtifactOrphans(t, f, request)
			report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
			if err != nil || report.entries != 0 || report.unlinked != 0 {
				t.Fatal("unproven ownership traversed the cohort", report, err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("unproven bytes were removed", err)
			}
		})
	}
}

func TestArtifactOrphansRetainsQueuedRunningAndExpiredMatchingWork(t *testing.T) {
	for _, state := range []string{"queued", "running-expired", "cancel-requested", "dedupe-only", "binding-only", "unknown-kind-binding"} {
		t.Run(state, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			request := artifactOrphanRequestForTest(t, f)
			_, file := writeArtifactOrphan(t, f, request, nil)
			tx, err := f.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			work, _ := stillInputWorkKey(request.MediaID, request.RevisionID, request.Preset)
			active, _, err := f.queue.EnqueueTx(t.Context(), tx, jobs.Input{Kind: "variant_still", ResourceKey: work, Dedupe: work, Payload: json.RawMessage(`{"version":1}`)})
			if err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "running-expired", "cancel-requested":
				if _, err := f.queue.Claim(t.Context(), "owned-live-producer", []string{"variant_still"}); err != nil {
					t.Fatal(err)
				}
			case "dedupe-only":
				if _, err := f.db.Exec(`UPDATE jobs SET kind='scan',resource_key='unrelated' WHERE id=?`, active.ID); err != nil {
					t.Fatal(err)
				}
			case "binding-only":
				if _, err := f.db.Exec(`UPDATE jobs SET dedupe_key='unrelated' WHERE id=?`, active.ID); err != nil {
					t.Fatal(err)
				}
			case "unknown-kind-binding":
				if _, err := f.db.Exec(`UPDATE jobs SET kind='scan',resource_key='unrelated',dedupe_key='unrelated' WHERE id=?`, active.ID); err != nil {
					t.Fatal(err)
				}
			}
			// Claim before forcing expiry: a later claim would legitimately recover it.
			lease := claimArtifactOrphans(t, f, request)
			if state == "running-expired" {
				if _, err := f.db.Exec(`UPDATE jobs SET lease_expires_at=0 WHERE id=?`, active.ID); err != nil {
					t.Fatal(err)
				}
			} else if state == "cancel-requested" {
				tx, err := f.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.queue.CancelResourceTx(t.Context(), tx, work, ""); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
			if err != nil || report.entries != 0 || report.unlinked != 0 {
				t.Fatal("matching active work was treated as abandoned", state, report, err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("live work's immutable bytes were removed", err)
			}
		})
	}
}

func TestArtifactOrphansRetainsUnknownUnsafeYoungCorruptAndNonFiniteFiles(t *testing.T) {
	for _, change := range []string{"unknown-name", "permissions", "hardlink", "symlink", "directory", "fifo", "young", "future", "oversize", "wrong-SHA", "invalid-container", "metadata-container"} {
		t.Run(change, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			request := artifactOrphanRequestForTest(t, f)
			_, file := writeArtifactOrphan(t, f, request, nil)
			old := time.Now().Add(-25 * time.Hour)
			switch change {
			case "unknown-name":
				name := filepath.Join(filepath.Dir(file), "unknown.webp")
				if err := os.Rename(file, name); err != nil {
					t.Fatal(err)
				}
				file = name
			case "permissions":
				if err := os.Chmod(file, 0o644); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(file, filepath.Join(t.TempDir(), "owned-link")); err != nil {
					t.Fatal(err)
				}
			case "symlink", "directory", "fifo":
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				var err error
				switch change {
				case "symlink":
					err = os.Symlink(f.file, file)
				case "directory":
					err = os.Mkdir(file, 0o700)
				case "fifo":
					err = syscall.Mkfifo(file, 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "young", "future":
				old = time.Now().Add(-time.Hour)
				if change == "future" {
					old = time.Now().Add(time.Hour)
				}
			case "oversize":
				if err := os.Truncate(file, (1<<20)+1); err != nil {
					t.Fatal(err)
				}
			case "wrong-SHA", "invalid-container", "metadata-container":
				data := []byte("RIFF\x04\x00\x00\x00WEBP")
				if change == "metadata-container" {
					data = encodeWebP(append(webpChunks(webpFixture(t, "lossless")), testWebPChunk{"XMP ", []byte("private metadata")}))
				}
				if err := os.WriteFile(file, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if change != "wrong-SHA" {
					digest := sha256.Sum256(data)
					name := filepath.Join(filepath.Dir(file), hex.EncodeToString(digest[:])+".webp")
					if err := os.Rename(file, name); err != nil {
						t.Fatal(err)
					}
					file = name
				}
			}
			if change != "symlink" {
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
			}
			lease := claimArtifactOrphans(t, f, request)
			report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
			if err != nil || report.entries != 1 || report.retained != 1 || report.unlinked != 0 {
				t.Fatal("unsafe/unknown bytes were treated as owned finite output", change, report, err)
			}
			if _, err := os.Lstat(file); err != nil {
				t.Fatal("retained entry disappeared", err)
			}
		})
	}
}

func TestArtifactOrphansDoesNotTraverseUnsafeParentsOrDifferentCohorts(t *testing.T) {
	for _, parentLevel := range []int{0, 1, 2, 3} {
		for _, alias := range []bool{false, true} {
			t.Run(fmt.Sprint(parentLevel, alias), func(t *testing.T) {
				f := artifactTempsFixture(t, true)
				request := artifactOrphanRequestForTest(t, f)
				_, file := writeArtifactOrphan(t, f, request, nil)
				parent := filepath.Dir(file)
				for range parentLevel {
					parent = filepath.Dir(parent)
				}
				if alias {
					if err := os.Rename(parent, parent+"-owned"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(parent+"-owned", parent); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Chmod(parent, 0o755); err != nil {
					t.Fatal(err)
				}
				lease := claimArtifactOrphans(t, f, request)
				report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
				if err != nil || report.entries != 0 || report.unlinked != 0 {
					t.Fatal("unsafe parent was traversed", report, err)
				}
				if _, err := os.Stat(file); err != nil {
					t.Fatal("unsafe parent's bytes disappeared", err)
				}
			})
		}
	}
	f := artifactTempsFixture(t, true)
	request := artifactOrphanRequestForTest(t, f)
	other := request
	other.RevisionID = "unknown-revision"
	_, file := writeArtifactOrphan(t, f, other, nil)
	lease := claimArtifactOrphans(t, f, request)
	report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
	if err != nil || report.entries != 0 || report.unlinked != 0 {
		t.Fatal("single-cohort cleanup discovered another revision", report, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("unknown cohort was removed", err)
	}
}

func TestArtifactOrphansLimitsFailTheScopeWithoutInventingCompleteSweep(t *testing.T) {
	for _, limit := range []string{"entry", "bytes"} {
		t.Run(limit, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			request := artifactOrphanRequestForTest(t, f)
			artifact, _ := writeArtifactOrphan(t, f, request, nil)
			if limit == "entry" {
				writeArtifactOrphan(t, f, request, webpFixture(t, "lossy"))
				request.EntryLimit = 1
			} else {
				request.ByteLimit = artifact.size - 1
			}
			lease := claimArtifactOrphans(t, f, request)
			report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
			if err != nil || !report.limited || report.entries != 1 || report.bytes > request.ByteLimit || report.unlinked > 1 {
				t.Fatal("bounded work was not reported honestly", report, err)
			}
			job, err := f.queue.Get(t.Context(), lease.Job.ID)
			if err != nil || job.Total != 1 || job.Done != 1 || job.Failed != 1 || job.Succeeded != 0 {
				t.Fatal("limited cohort counted as a successful full sweep", job, err)
			}
		})
	}
}

func TestArtifactOrphansRejectsChangedCleanupIdentityBindingsAndLeaseBeforeIO(t *testing.T) {
	for name, query := range map[string]string{
		"kind":             `UPDATE jobs SET kind='reconcile_temps' WHERE id=?`,
		"resource":         `UPDATE jobs SET resource_key='unrelated' WHERE id=?`,
		"token":            `UPDATE jobs SET dedupe_token='unrelated' WHERE id=?`,
		"payload":          `UPDATE jobs SET payload_json='{"version":2}' WHERE id=?`,
		"primary binding":  `DELETE FROM job_resources WHERE job_id=? AND resource_key GLOB 'artifact_orphan:*'`,
		"media binding":    `DELETE FROM job_resources WHERE job_id=? AND resource_key='media:input-media'`,
		"revision binding": `DELETE FROM job_resources WHERE job_id=? AND resource_key='revision:input-revision'`,
		"cancel":           `UPDATE jobs SET cancel_requested=1 WHERE id=?`,
		"expiry":           `UPDATE jobs SET lease_expires_at=0 WHERE id=?`,
		"owner":            `UPDATE jobs SET lease_owner='unrelated' WHERE id=?`,
		"attempt":          `UPDATE jobs SET attempts=attempts+1 WHERE id=?`,
		"consumed budget":  `UPDATE jobs SET total_known=1 WHERE id=?`,
		"source warning":   `UPDATE sources SET state='unavailable' WHERE id=(SELECT 'input-source' FROM jobs WHERE id=?)`,
	} {
		t.Run(name, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			request := artifactOrphanRequestForTest(t, f)
			_, file := writeArtifactOrphan(t, f, request, nil)
			lease := claimArtifactOrphans(t, f, request)
			if _, err := f.db.Exec(query, lease.Job.ID); err != nil {
				t.Fatal(err)
			}
			report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
			if err == nil || report.entries != 0 || report.unlinked != 0 {
				t.Fatal("invalid cleanup scope reached filesystem work", name, report, err)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatal("preflight rejection removed bytes", err)
			}
		})
	}
}

func TestArtifactOrphansRechecksLateBindingsReferencesAndJournalFences(t *testing.T) {
	for _, boundary := range []string{"budget-media", "heartbeat-revision", "heartbeat-reference", "scope-binding", "scope-audit", "scope-outbox", "finish-binding", "completion-binding", "completion-identity", "completion-source"} {
		t.Run(boundary, func(t *testing.T) {
			f := artifactTempsFixture(t, true)
			request := artifactOrphanRequestForTest(t, f)
			artifact, file := writeArtifactOrphan(t, f, request, nil)
			lease := claimArtifactOrphans(t, f, request)
			var query string
			switch boundary {
			case "budget-media":
				query = fmt.Sprintf(`CREATE TRIGGER orphan_test_fence AFTER UPDATE OF total_known ON jobs WHEN NEW.id='%s' AND OLD.total_known=0 AND NEW.total_known=1 BEGIN DELETE FROM job_resources WHERE job_id=NEW.id AND resource_key='media:input-media'; END`, lease.Job.ID)
			case "heartbeat-revision", "heartbeat-reference":
				action := `DELETE FROM job_resources WHERE job_id=NEW.id AND resource_key='revision:input-revision' AND (SELECT n FROM orphan_test_heartbeats)=2;`
				if boundary == "heartbeat-reference" {
					action = fmt.Sprintf(`INSERT INTO media_variants(id,revision_id,preset_key,preset_version,storage_path,mime,size_bytes,sha256,status,created_at) SELECT 'late-reference','input-revision','reference',1,'%s','image/webp',1,'%s','retired',0 WHERE (SELECT n FROM orphan_test_heartbeats)=2;`, artifact.storage, artifact.hash)
				}
				query = fmt.Sprintf(`CREATE TABLE orphan_test_heartbeats(n INTEGER); INSERT INTO orphan_test_heartbeats VALUES(0); CREATE TRIGGER orphan_test_fence AFTER UPDATE OF heartbeat_at ON jobs WHEN NEW.id='%s' BEGIN UPDATE orphan_test_heartbeats SET n=n+1; %s END`, lease.Job.ID, action)
			case "scope-binding", "finish-binding":
				event := "artifact_orphan_scope_checked"
				if boundary == "finish-binding" {
					event = "artifact_orphans_reconciled"
				}
				query = fmt.Sprintf(`CREATE TRIGGER orphan_test_fence AFTER INSERT ON audit_events WHEN NEW.event_type='%s' BEGIN DELETE FROM job_resources WHERE job_id='%s' AND resource_key='revision:input-revision'; END`, event, lease.Job.ID)
			case "completion-binding", "completion-identity", "completion-source":
				action := `DELETE FROM job_resources WHERE job_id=NEW.id AND resource_key='revision:input-revision';`
				if boundary == "completion-identity" {
					action = `UPDATE jobs SET payload_json='{"version":2}' WHERE id=NEW.id;`
				} else if boundary == "completion-source" {
					action = `UPDATE sources SET state='unavailable';`
				}
				query = fmt.Sprintf(`CREATE TRIGGER orphan_test_fence AFTER UPDATE OF status ON jobs WHEN NEW.id='%s' AND NEW.status='succeeded' BEGIN %s END`, lease.Job.ID, action)
			case "scope-audit":
				query = `CREATE TRIGGER orphan_test_fence BEFORE INSERT ON audit_events WHEN NEW.event_type='artifact_orphan_scope_checked' BEGIN SELECT RAISE(ABORT,'injected orphan audit failure'); END`
			case "scope-outbox":
				query = fmt.Sprintf(`CREATE TRIGGER orphan_test_fence BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log c WHERE c.seq=NEW.change_seq AND c.resource_id='%s' AND json_extract(c.payload_json,'$.done')=1) BEGIN SELECT RAISE(ABORT,'injected orphan outbox failure'); END`, lease.Job.ID)
			}
			if _, err := f.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			report, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request)
			if boundary == "heartbeat-reference" {
				if err != nil || report.unlinked != 0 {
					t.Fatal("late reference was not retained", report, err)
				}
			} else if err == nil {
				t.Fatal("late cleanup fence was accepted", boundary, report)
			}
			job, jobErr := f.queue.Get(t.Context(), lease.Job.ID)
			if jobErr != nil {
				t.Fatal(jobErr)
			}
			_, statErr := os.Stat(file)
			if strings.HasPrefix(boundary, "scope-") || boundary == "finish-binding" || strings.HasPrefix(boundary, "completion-") {
				if report.unlinked != 1 || !errors.Is(statErr, os.ErrNotExist) {
					t.Fatal("fixture did not reach the physical unlink boundary", report, statErr)
				}
				if strings.HasPrefix(boundary, "scope-") && (job.Done != 0 || job.Succeeded != 0 || job.Total != 1) {
					t.Fatal("unconfirmed unlink escaped transaction rollback", job)
				}
				if (boundary == "finish-binding" || strings.HasPrefix(boundary, "completion-")) && job.Status != "running" {
					t.Fatal("late completion fence committed terminal state", job)
				}
			} else if statErr != nil {
				t.Fatal("pre-unlink rejection lost bytes", statErr)
			}
			if boundary != "heartbeat-reference" {
				if _, err := f.svc.reconcileArtifactOrphans(t.Context(), f.queue, lease, request); !errors.Is(err, jobs.ErrState) {
					// Budget rejection rolls back its reservation; its persistent test
					// trigger rejects the next call too, without permitting any IO.
					if boundary != "budget-media" {
						t.Fatal("same-attempt work expanded or repeated", err)
					}
				}
			}
			var orphan, completed int
			if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log c WHERE NOT EXISTS(SELECT 1 FROM outbox_events o WHERE o.change_seq=c.seq)),(SELECT COUNT(*) FROM audit_events WHERE event_type='artifact_orphans_reconciled')`).Scan(&orphan, &completed); err != nil || orphan != 0 || (boundary != "heartbeat-reference" && completed != 0) {
				t.Fatal("late failure left a partial change/outbox/final audit", orphan, completed, err)
			}
		})
	}
}

func TestArtifactOrphansCancellationWhileWaitingDoesNotHoldSQLiteWriter(t *testing.T) {
	f := artifactTempsFixture(t, true)
	request := artifactOrphanRequestForTest(t, f)
	_, file := writeArtifactOrphan(t, f, request, nil)
	lease := claimArtifactOrphans(t, f, request)
	shared, err := f.svc.lockArtifacts(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := f.svc.reconcileArtifactOrphans(ctx, f.queue, lease, request); done <- err }()
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
			t.Fatal("immutable cleanup did not wait for shared storage", err)
		case <-deadline.C:
			t.Fatal("immutable cleanup did not open its gate")
		case <-tick.C:
		}
	}
	writerCtx, writerCancel := context.WithTimeout(t.Context(), time.Second)
	defer writerCancel()
	tx, err := f.db.BeginTx(writerCtx, nil)
	if err != nil {
		t.Fatal("gate wait blocked an independent SQLite writer", err)
	}
	tx.Rollback()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("gate waiter did not honor cancellation", err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.TotalKnown || job.Total != 0 {
		t.Fatal("gate waiter consumed an attempt budget", job, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("cancelled gate waiter removed bytes", err)
	}
}
