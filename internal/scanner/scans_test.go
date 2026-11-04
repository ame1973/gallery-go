package scanner

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gallerygo/internal/auth"
	"gallerygo/internal/library"
	"gallerygo/internal/store"
)

func scanID(value string) *string { return &value }

func TestScanEnqueueSnapshotsTargetsAndReplaysHistoricalReceipts(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	if err := os.Remove(filepath.Join(f.rootPath, ".mounted")); err != nil {
		t.Fatal(err)
	}
	request := ScanRequest{SourceID: &f.source.ID, Mode: "full"}
	key := store.NewID()
	job, err := scans.Enqueue(t.Context(), f.owner, request, key)
	if err != nil || job.Status != "queued" || job.TotalKnown {
		t.Fatal(job, err)
	}
	var source, scope string
	var config int64
	if err := f.db.QueryRow(`SELECT source_id,source_config_version,scope_path FROM scan_targets WHERE job_id=?`, job.ID).Scan(&source, &config, &scope); err != nil || source != f.source.ID || config != 1 || scope != "." {
		t.Fatal("target snapshot", source, config, scope, err)
	}
	encoded, _ := json.Marshal(job)
	for _, private := range []string{f.rootPath, "scan_targets", "scope_path", "source_config_version"} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("private scan selection leaked", private)
		}
	}
	if _, err := os.Stat(filepath.Join(f.rootPath, ".mounted")); !os.IsNotExist(err) {
		t.Fatal("enqueue touched or created a mount marker", err)
	}
	minimum := int64(3)
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{MinimumFiles: &minimum})
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.queue.Get(t.Context(), job.ID)
	if err != nil || current.Status != "cancelled" {
		t.Fatal("changed source did not cancel accepted intent", current, err)
	}
	replay, err := scans.Enqueue(t.Context(), f.owner, request, key)
	if err != nil || replay.ID != job.ID || replay.Status != "queued" || replay.Version != job.Version {
		t.Fatal("receipt failed to return historical accepted result", replay, err)
	}
	fresh, err := scans.Enqueue(t.Context(), f.owner, request, store.NewID())
	if err != nil || fresh.ID == job.ID {
		t.Fatal("new source epoch reused an old intent", fresh, err)
	}
	changed := request
	changed.Mode = "incremental"
	_, err = scans.Enqueue(t.Context(), f.owner, changed, key)
	walkError(t, err, store.ErrIdempotencyReuse)
	if err := f.sources.DeleteEmpty(t.Context(), f.owner, f.source.ID, f.source.Version); err != nil {
		t.Fatal(err)
	}
	deletedReplay, err := scans.Enqueue(t.Context(), f.owner, request, key)
	if err != nil || deletedReplay.ID != job.ID {
		t.Fatal("registration deletion lost accepted-result replay", deletedReplay, err)
	}
	var history int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM scan_targets WHERE source_id=?`, source).Scan(&history); err != nil || history != 2 {
		t.Fatal("registration deletion erased scan target history", history, err)
	}
	_, err = scans.Enqueue(t.Context(), f.owner, request, store.NewID())
	walkError(t, err, sql.ErrNoRows)
}

func TestScanIntentsCoalesceOnlyIdenticalSelectionsModesAndEpochs(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	album := store.NewID()
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,relative_path,created_at,updated_at) VALUES(?,?,?,'Target','Album',0,0)`, album, f.source.LibraryID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	request := ScanRequest{SourceID: &f.source.ID, Mode: "full"}
	first, err := scans.Enqueue(t.Context(), f.owner, request, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	second, err := scans.Enqueue(t.Context(), f.owner, request, store.NewID())
	if err != nil || second.ID != first.ID {
		t.Fatal("identical active intent did not coalesce", second, err)
	}
	ids := map[string]bool{first.ID: true}
	for _, other := range []ScanRequest{{Mode: "full"}, {SourceID: &f.source.ID, Mode: "incremental"}, {AlbumID: &album, Mode: "full"}} {
		job, err := scans.Enqueue(t.Context(), f.owner, other, store.NewID())
		if err != nil || ids[job.ID] {
			t.Fatal("different scan intents coalesced", other, job, err)
		}
		ids[job.ID] = true
	}
	constrained, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{SourceID: &f.source.ID, AlbumID: &album, Mode: "full"}, store.NewID())
	if err != nil || !ids[constrained.ID] {
		t.Fatal("matching source constraint changed effective album intent", constrained, err)
	}
	var source, scope string
	var targetAlbum sql.NullString
	if err := f.db.QueryRow(`SELECT source_id,scope_path,album_id FROM scan_targets WHERE job_id=?`, constrained.ID).Scan(&source, &scope, &targetAlbum); err != nil || source != f.source.ID || scope != "Album" || targetAlbum.String != album {
		t.Fatal(source, scope, targetAlbum, err)
	}
	var targets int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM scan_targets`).Scan(&targets); err != nil || targets != 4 {
		t.Fatal("coalescing expanded immutable targets", targets, err)
	}
}

func TestScanEnqueueAllSourcesStreamsLargeSetsWithoutJSONOrFilesystemExpansion(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	for index := range 201 {
		id := store.NewID()
		if _, err := f.db.Exec(`INSERT INTO sources(id,library_id,name,root_path,mapping_json,is_active,created_at) VALUES(?,?,?,?,?,1,0)`, id, f.source.LibraryID, fmt.Sprintf("Unconfigured %d", index), filepath.Join(t.TempDir(), "never-created"), `{"album_depth":1,"person_depth":0}`); err != nil {
			t.Fatal(err)
		}
	}
	job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "incremental"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	var targets, resources, payloadBytes int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM scan_targets WHERE job_id=?),(SELECT COUNT(*) FROM job_resources WHERE job_id=?),length(payload_json) FROM jobs WHERE id=?`, job.ID, job.ID, job.ID).Scan(&targets, &resources, &payloadBytes); err != nil || targets != 202 || resources != 203 || payloadBytes > 64 {
		t.Fatal("all-source selection expanded payload or omitted rows", targets, resources, payloadBytes, err)
	}
}

func TestScanEnqueueLateAuditFailureRollsBackTargetsDependenciesReceiptAndJob(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_scan_audit BEFORE INSERT ON audit_events WHEN NEW.event_type='scan_enqueued' BEGIN SELECT RAISE(ABORT,'injected final scan audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID()); err == nil {
		t.Fatal("scan enqueue survived a final audit failure")
	}
	var count int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM jobs)+(SELECT COUNT(*) FROM scan_targets)+(SELECT COUNT(*) FROM job_resources)+(SELECT COUNT(*) FROM mutation_receipts WHERE operation='scan.enqueue')`).Scan(&count); err != nil || count != 0 {
		t.Fatal("partial scan intent survived rollback", count, err)
	}
}

func TestScanSelectionValidationAndFreshAdministrativeAuthorization(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	for _, request := range []ScanRequest{{}, {Mode: "FULL"}, {Mode: "full", SourceID: scanID("")}, {Mode: "full", AlbumID: scanID(strings.Repeat("a", 65))}} {
		_, err := scans.Enqueue(t.Context(), f.owner, request, store.NewID())
		walkError(t, err, ErrScanSelection)
	}
	_, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{SourceID: scanID("missing"), Mode: "full"}, store.NewID())
	walkError(t, err, sql.ErrNoRows)
	album := store.NewID()
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,relative_path,created_at,updated_at) VALUES(?,?,?,'Target','Album',0,0)`, album, f.source.LibraryID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	_, err = scans.Enqueue(t.Context(), f.owner, ScanRequest{SourceID: scanID("different"), AlbumID: &album, Mode: "full"}, store.NewID())
	walkError(t, err, ErrScanSelection)
	active := false
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{Active: &active})
	if err != nil {
		t.Fatal(err)
	}
	_, err = scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	walkError(t, err, ErrNoSources)
	_, err = scans.Enqueue(t.Context(), f.owner, ScanRequest{SourceID: &f.source.ID, Mode: "full"}, store.NewID())
	walkError(t, err, ErrScanSelection)
	if _, err := f.db.Exec(`UPDATE users SET role='user' WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	_, err = scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
	walkError(t, err, auth.ErrForbidden)
}

func TestConcurrentScanEnqueueUsesOneImmutableActiveIntent(t *testing.T) {
	f := inventorySetup(t)
	scans := NewScans(f.db, f.auth, f.queue)
	var wg sync.WaitGroup
	ids := make(chan string, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			job, err := scans.Enqueue(t.Context(), f.owner, ScanRequest{Mode: "full"}, store.NewID())
			ids <- job.ID
			failures <- err
		})
	}
	wg.Wait()
	close(ids)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != 1 {
		t.Fatal("concurrent requests duplicated an active scan", unique)
	}
	var targets int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM scan_targets`).Scan(&targets); err != nil || targets != 1 {
		t.Fatal("duplicate intent expanded targets", targets, err)
	}
}
