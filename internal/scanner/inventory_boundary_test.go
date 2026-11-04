package scanner

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gallerygo/internal/jobs"
	"gallerygo/internal/media"
)

func TestReadyFenceEmptyInventoryIdempotencyAndSourceDeletion(t *testing.T) {
	f := inventorySetup(t)
	lease := f.claim(t, "scan:empty")
	generation, err := f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = f.inventory.RequireReadyTx(t.Context(), tx, lease, generation.ID)
	walkError(t, err, ErrGeneration)
	tx.Rollback()
	ready, err := f.inventory.Build(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil || ready.ID != generation.ID || ready.FileCount != 0 || !ready.Healthy {
		t.Fatal(ready, err)
	}
	replay, err := f.inventory.Build(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil || replay.Version != ready.Version {
		t.Fatal("ready replay repeated staging", replay, err)
	}
	tx, err = f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, root, err := f.inventory.RequireReadyTx(t.Context(), tx, lease, ready.ID)
	if err != nil || root.Path != f.rootPath {
		t.Fatal("ready publisher fence", root, err)
	}
	tx.Rollback()
	if strings.Contains(fmt.Sprint(ready), lease.Owner) {
		t.Fatal("generation diagnostic exposed private owner")
	}
	if err := f.sources.DeleteEmpty(t.Context(), f.owner, f.source.ID, f.source.Version); err != nil {
		t.Fatal(err)
	}
	_, err = f.inventory.Get(t.Context(), ready.ID)
	walkError(t, err, sql.ErrNoRows)
	_, err = f.inventory.Seal(t.Context(), lease, ready.ID)
	walkError(t, err, jobs.ErrCancelled)
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM job_attempts WHERE job_id=?`, lease.Job.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("source removal deleted job history", count, err)
	}
}

func TestSealRejectsAddedAndDeletedPaths(t *testing.T) {
	for _, change := range []string{"add", "delete"} {
		t.Run(change, func(t *testing.T) {
			f := inventorySetup(t)
			root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
			original := writeInput(t, root, "one", []byte("one"))
			lease := f.claim(t, "scan:path-change")
			generation, err := f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.inventory.Record(t.Context(), lease, generation.ID, f.files(t)); err != nil {
				t.Fatal(err)
			}
			if change == "add" {
				writeInput(t, root, "two", []byte("two"))
			} else if err := os.Remove(original); err != nil {
				t.Fatal(err)
			}
			_, err = f.inventory.Seal(t.Context(), lease, generation.ID)
			walkError(t, err, ErrChanged)
			current, err := f.inventory.Get(t.Context(), generation.ID)
			if err != nil || current.State != "walking" || current.Healthy {
				t.Fatal("incomplete snapshot became ready", current, err)
			}
		})
	}
}

func TestInventoryBeginSealAbortAuditFailuresAreAtomic(t *testing.T) {
	f := inventorySetup(t)
	lease := f.claim(t, "scan:begin-rollback")
	if _, err := f.db.Exec(`CREATE TRIGGER fail_inventory_audit BEFORE INSERT ON audit_events WHEN NEW.resource_type='inventory' BEGIN SELECT RAISE(ABORT,'injected inventory audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", "."); err == nil {
		t.Fatal("generation began without audit")
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inventory_generations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial generation creation", count, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_inventory_audit; UPDATE sources SET scan_generation=5`); err != nil {
		t.Fatal(err)
	}
	generation, err := f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil || generation.Number != 6 {
		t.Fatal("generation numbering moved backward", generation, err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_inventory_seal BEFORE INSERT ON outbox_events WHEN (SELECT kind FROM change_log WHERE seq=NEW.change_seq)='inventory' BEGIN SELECT RAISE(ABORT,'injected seal event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.Seal(t.Context(), lease, generation.ID); err == nil {
		t.Fatal("generation sealed without event")
	}
	current, err := f.inventory.Get(t.Context(), generation.ID)
	if err != nil || current.Healthy || current.State != "walking" {
		t.Fatal("partial healthy seal", current, err)
	}
	if err := f.inventory.Abort(t.Context(), lease, generation.ID, "IO_ERROR", "."); err == nil {
		t.Fatal("generation failure committed without event")
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inventory_issues`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failure issue survived rollback", count, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_inventory_seal`); err != nil {
		t.Fatal(err)
	}
	walkError(t, f.inventory.Abort(t.Context(), lease, generation.ID, "/private/root", "."), ErrGeneration)
	walkError(t, f.inventory.Abort(t.Context(), lease, generation.ID, "IO_ERROR", "../outside"), ErrGeneration)
	_, err = f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "invented", ".")
	walkError(t, err, ErrGeneration)
	_, err = f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", "../outside")
	walkError(t, err, ErrGeneration)
	if err := f.inventory.Abort(t.Context(), lease, generation.ID, "IO_ERROR", "."); err != nil {
		t.Fatal(err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Failed != 1 || job.Succeeded != 0 || job.Done != 1 {
		t.Fatal("root failure invented file success", job, err)
	}
	if _, err := os.Stat(filepath.Join(f.rootPath, ".mounted")); err != nil {
		t.Fatal("failure touched source marker", err)
	}
}
