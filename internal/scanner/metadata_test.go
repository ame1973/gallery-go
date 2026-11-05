package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func metadataPNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewNRGBA(image.Rect(0, 0, 12, 7))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func (f inventoryFixture) ready(t *testing.T) (jobs.Lease, Generation) {
	t.Helper()
	lease := f.claim(t, "scan:metadata")
	generation, err := f.inventory.Build(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil {
		t.Fatal(err)
	}
	return lease, generation
}

func TestMetadataStagesEveryPathAndExplicitFailuresWithoutPublishing(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	original := metadataPNG(t)
	name := writeInput(t, root, "a.mp4", original)
	writeInput(t, root, "b.jpg", []byte("not a valid JPEG"))
	writeInput(t, root, "c.webm", []byte("\x1a\x45\xdf\xa3\x87\x42\x82\x84webm"))
	writeInput(t, root, "d.txt", []byte("not media"))
	lease, ready := f.ready(t)
	policy := media.DefaultInspectPolicy()
	staged, err := f.inventory.ProbeMetadata(t.Context(), lease, ready.ID, policy)
	if err != nil || !staged.MetadataComplete || staged.MetadataCount != 4 || staged.State != "ready" || !staged.Healthy {
		t.Fatal(staged, err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Done != 8 || job.Total != 8 || job.Succeeded != 5 || job.Failed != 2 || job.Skipped != 1 || job.TotalKnown || job.Status != "running" {
		t.Fatal("metadata progress did not retain explicit outcomes", job, err)
	}
	for path, status := range map[string]string{"a.mp4": "ok", "b.jpg": "invalid", "c.webm": "invalid", "d.txt": "not_media"} {
		result, err := readInspection(f.db.QueryRow(`SELECT inspection_json FROM inventory_media WHERE generation_id=? AND relative_path=?`, ready.ID, path))
		if err != nil || result.Status != status {
			t.Fatal(path, result, err)
		}
		if path == "a.mp4" && (result.Kind != "image" || result.MIME != "image/png" || result.Width == nil || *result.Width != 12 || result.Height == nil || *result.Height != 7 || result.Animated == nil || *result.Animated || result.FrameCount == nil || *result.FrameCount != 1) {
			t.Fatal("extension overrode real PNG metadata", result)
		}
	}
	var issues, indexed, published int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM inventory_issues),(SELECT COUNT(*) FROM media)+(SELECT COUNT(*) FROM albums)+(SELECT COUNT(*) FROM source_entries),(SELECT scan_generation FROM sources WHERE id=?)`, f.source.ID).Scan(&issues, &indexed, &published); err != nil || issues != 2 || indexed != 0 || published != 0 {
		t.Fatal("metadata changed the index or omitted issues", issues, indexed, published, err)
	}
	replay, err := f.inventory.ProbeMetadata(t.Context(), lease, ready.ID, policy)
	if err != nil || replay.Version != staged.Version {
		t.Fatal("completed metadata replay mutated generation", replay, err)
	}
	files, err := f.inventory.fileBatch(t.Context(), ready.ID, "", f.inventory.policy.BatchSize)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := readInspection(f.db.QueryRow(`SELECT inspection_json FROM inventory_media WHERE generation_id=? AND relative_path=?`, ready.ID, files[0].RelativePath))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := f.inventory.recordMetadata(t.Context(), lease, ready.ID, []inspectedFile{{files[0], inspection}})
	if err != nil || replayed.Version != staged.Version {
		t.Fatal("immutable batch replay duplicated progress", replayed, err)
	}
	wrongWidth := int64(99)
	inspection.Width = &wrongWidth
	_, err = f.inventory.recordMetadata(t.Context(), lease, ready.ID, []inspectedFile{{files[0], inspection}})
	walkError(t, err, ErrChanged)
	policy.MaxPixels--
	_, err = f.inventory.ProbeMetadata(t.Context(), lease, ready.ID, policy)
	walkError(t, err, ErrGeneration)
	encoded, _ := json.Marshal(staged)
	for _, private := range []string{f.rootPath, lease.Owner, staged.inspectionPolicy, "inspect_policy_sha256"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("private processing evidence leaked", private)
		}
	}
	actual, err := os.ReadFile(name)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatal("metadata wrote to source input", err)
	}
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, _, _, err = f.inventory.RequireMetadataTx(t.Context(), tx, lease, ready.ID)
	if err != nil {
		t.Fatal("complete metadata did not pass live publisher fence", err)
	}
}

func TestMetadataEmptyInventoryRequiresHealthPolicyAndCompletion(t *testing.T) {
	f := inventorySetup(t)
	lease := f.claim(t, "scan:empty-metadata")
	generation, err := f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, media.DefaultInspectPolicy())
	walkError(t, err, ErrGeneration)
	generation, err = f.inventory.Build(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = f.inventory.RequireMetadataTx(t.Context(), tx, lease, generation.ID)
	walkError(t, err, ErrGeneration)
	tx.Rollback()
	_, err = f.inventory.completeMetadata(t.Context(), lease, generation.ID)
	walkError(t, err, ErrGeneration)
	policy := media.DefaultInspectPolicy()
	invalid := policy
	invalid.HeaderBytes = 0
	_, err = f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, invalid)
	walkError(t, err, media.ErrInspectPolicy)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.inventory.ProbeMetadata(ctx, lease, generation.ID, policy)
	walkError(t, err, context.Canceled)
	marker := filepath.Join(f.rootPath, ".mounted")
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, policy); err == nil {
		t.Fatal("empty inventory bypassed missing mount marker")
	}
	current, err := f.inventory.Get(t.Context(), generation.ID)
	if err != nil || current.MetadataComplete {
		t.Fatal("outage marked metadata complete", current, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("missing marker was recreated", err)
	}
	if err := os.WriteFile(marker, []byte("restored test marker"), 0o400); err != nil {
		t.Fatal(err)
	}
	complete, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, policy)
	if err != nil || !complete.MetadataComplete || complete.MetadataCount != 0 {
		t.Fatal(complete, err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Done != 0 || job.Succeeded != 0 || job.TotalKnown {
		t.Fatal("empty metadata invented work", job, err)
	}
}

func TestMetadataProgressAndCompletionFailuresRollBackWithoutDoubleCounting(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "a.jpg", []byte("invalid"))
	writeInput(t, root, "b.png", metadataPNG(t))
	lease, generation := f.ready(t)
	policy, err := media.DefaultInspectPolicy().PinCodecRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pinned, _, err := f.inventory.beginMetadata(t.Context(), lease, generation.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_metadata_progress BEFORE INSERT ON outbox_events WHEN (SELECT kind FROM change_log WHERE seq=NEW.change_seq)='job' BEGIN SELECT RAISE(ABORT,'injected late metadata progress failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, policy); err == nil {
		t.Fatal("metadata survived failed progress event")
	}
	var count int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM inventory_media)+(SELECT COUNT(*) FROM inventory_issues)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("metadata/issues escaped transaction rollback", count, err)
	}
	current, err := f.inventory.Get(t.Context(), generation.ID)
	if err != nil || current.Version != pinned.Version || current.MetadataCount != 0 || current.MetadataComplete {
		t.Fatal("partial metadata generation", current, err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Done != 2 || job.Succeeded != 2 || job.Failed != 0 {
		t.Fatal("partial job progress", job, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_metadata_progress; CREATE TRIGGER fail_metadata_completion BEFORE INSERT ON audit_events WHEN NEW.event_type='inventory_metadata_staged' BEGIN SELECT RAISE(ABORT,'injected completion audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, policy); err == nil {
		t.Fatal("completion survived failed audit")
	}
	current, err = f.inventory.Get(t.Context(), generation.ID)
	if err != nil || current.MetadataCount != 2 || current.MetadataComplete {
		t.Fatal("completed flag escaped audit rollback", current, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_metadata_completion`); err != nil {
		t.Fatal(err)
	}
	complete, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, policy)
	if err != nil || !complete.MetadataComplete || complete.Version != current.Version+1 {
		t.Fatal("retry did not resume immutable batches", complete, err)
	}
	job, err = f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Done != 4 || job.Succeeded != 3 || job.Failed != 1 {
		t.Fatal("retry duplicated completed metadata work", job, err)
	}
}

func TestMetadataRejectsChangedAndUnsafeInputsAfterSealing(t *testing.T) {
	for _, change := range []string{"equal-stat", "replacement", "escape", "fifo"} {
		t.Run(change, func(t *testing.T) {
			f := inventorySetup(t)
			root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
			data := append(metadataPNG(t), bytes.Repeat([]byte("a"), hashChunk*3)...)
			name := writeInput(t, root, "photo.png", data)
			lease, generation := f.ready(t)
			info, err := os.Stat(name)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "equal-stat":
				data[len(data)-1] = 'b'
				if err := os.Chmod(name, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, data, 0o400); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				replacement := writeInput(t, root, "replacement", data)
				if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, name); err != nil {
					t.Fatal(err)
				}
			case "escape", "fifo":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if change == "fifo" {
					if err := syscall.Mkfifo(name, 0o600); err != nil {
						t.Fatal(err)
					}
				} else {
					outside := filepath.Join(t.TempDir(), "outside.png")
					if err := os.WriteFile(outside, data, 0o400); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(outside, name); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err = f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, media.DefaultInspectPolicy())
			if err == nil {
				t.Fatal("unsafe source result was staged", change)
			}
			if change == "equal-stat" || change == "replacement" {
				walkError(t, err, ErrChanged)
			}
			current, err := f.inventory.Get(t.Context(), generation.ID)
			if err != nil || current.MetadataComplete || current.MetadataCount != 0 {
				t.Fatal(current, err)
			}
			var count int
			if err := f.db.QueryRow(`SELECT COUNT(*) FROM inventory_media`).Scan(&count); err != nil || count != 0 {
				t.Fatal("changed input metadata persisted", count, err)
			}
		})
	}
}

func TestCompletedMetadataStillRequiresCurrentLeaseAndSourceConfiguration(t *testing.T) {
	f := inventorySetup(t)
	lease, generation := f.ready(t)
	policy := media.DefaultInspectPolicy()
	if _, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, policy); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"owner", "attempt", "job", "expiry", "config", "inactive", "cancel"} {
		t.Run(change, func(t *testing.T) {
			current := lease
			want := jobs.ErrLeaseLost
			switch change {
			case "owner":
				current.Owner = store.NewID()
			case "attempt":
				current.Job.Attempts++
			case "job":
				current.Job.ID = store.NewID()
			case "expiry":
				var expiry int64
				if err := f.db.QueryRow(`SELECT lease_expires_at FROM jobs WHERE id=?`, lease.Job.ID).Scan(&expiry); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE jobs SET lease_expires_at=0 WHERE id=?`, lease.Job.ID); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := f.db.Exec(`UPDATE jobs SET lease_expires_at=? WHERE id=?`, expiry, lease.Job.ID); err != nil {
						t.Error(err)
					}
				})
			case "config", "inactive":
				want = library.ErrSourceChanged
				if change == "config" {
					if _, err := f.db.Exec(`UPDATE sources SET config_version=config_version+1 WHERE id=?`, f.source.ID); err != nil {
						t.Fatal(err)
					}
				} else if _, err := f.db.Exec(`UPDATE sources SET is_active=0 WHERE id=?`, f.source.ID); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := f.db.Exec(`UPDATE sources SET config_version=?,is_active=1 WHERE id=?`, f.source.ConfigVersion, f.source.ID); err != nil {
						t.Error(err)
					}
				})
			case "cancel":
				want = jobs.ErrCancelled
				if _, err := f.db.Exec(`UPDATE jobs SET cancel_requested=1 WHERE id=?`, lease.Job.ID); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := f.db.Exec(`UPDATE jobs SET cancel_requested=0 WHERE id=?`, lease.Job.ID); err != nil {
						t.Error(err)
					}
				})
			}
			_, err := f.inventory.ProbeMetadata(t.Context(), current, generation.ID, policy)
			walkError(t, err, want)
			tx, err := f.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			_, _, _, err = f.inventory.RequireMetadataTx(t.Context(), tx, current, generation.ID)
			walkError(t, err, want)
		})
	}
}

func TestMetadataRejectsIncompleteInvalidAndUnboundResults(t *testing.T) {
	f := inventorySetup(t)
	writeInput(t, media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}, "photo.png", metadataPNG(t))
	lease, generation := f.ready(t)
	files, err := f.inventory.fileBatch(t.Context(), generation.ID, "", 2)
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	width, height := int64(12), int64(7)
	codec := "png"
	result := media.Inspection{Kind: "image", MIME: "image/png", Format: "png", Codec: &codec, Width: &width, Height: &height, Status: "ok"}
	_, err = f.inventory.recordMetadata(t.Context(), lease, generation.ID, []inspectedFile{{files[0], result}})
	walkError(t, err, ErrGeneration)
	if _, _, err := f.inventory.beginMetadata(t.Context(), lease, generation.ID, media.DefaultInspectPolicy()); !errors.Is(err, ErrGeneration) {
		t.Fatal("metadata accepted an unfrozen codec policy", err)
	}
	policy, err := media.DefaultInspectPolicy().PinCodecRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.inventory.beginMetadata(t.Context(), lease, generation.ID, policy); err != nil {
		t.Fatal(err)
	}
	_, err = f.inventory.completeMetadata(t.Context(), lease, generation.ID)
	walkError(t, err, ErrGeneration)
	_, err = f.inventory.recordMetadata(t.Context(), lease, generation.ID, nil)
	walkError(t, err, ErrLimit)
	_, err = f.inventory.recordMetadata(t.Context(), lease, generation.ID, make([]inspectedFile, 3))
	walkError(t, err, ErrLimit)
	_, err = f.inventory.recordMetadata(t.Context(), lease, generation.ID, []inspectedFile{{files[0], media.Inspection{}}})
	walkError(t, err, ErrUnsafeEntry)
	changed := files[0]
	changed.SHA256 = strings.Repeat("0", 64)
	_, err = f.inventory.recordMetadata(t.Context(), lease, generation.ID, []inspectedFile{{changed, result}})
	walkError(t, err, ErrChanged)
	if _, err := f.db.Exec(`UPDATE jobs SET total=9223372036854775807,done=9223372036854775807,succeeded=9223372036854775807 WHERE id=?`, lease.Job.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.inventory.recordMetadata(t.Context(), lease, generation.ID, []inspectedFile{{files[0], result}})
	walkError(t, err, ErrLimit)
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inventory_media`).Scan(&count); err != nil || count != 0 {
		t.Fatal("overflow kept partial results", count, err)
	}
}
