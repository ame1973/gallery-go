package scanner

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

type inventoryFixture struct {
	db        *sql.DB
	queue     *jobs.Queue
	sources   *library.Sources
	inventory *Inventory
	owner     auth.Session
	auth      *auth.Service
	source    library.Source
	rootPath  string
}

func inventorySetup(t *testing.T) inventoryFixture {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(db, auth.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	const password = "a private inventory integration test phrase"
	if _, err := a.Bootstrap(t.Context(), "owner", password); err != nil {
		t.Fatal(err)
	}
	owner, err := a.Login(t.Context(), "owner", password, "192.0.2.1", "inventory tests", "")
	if err != nil {
		t.Fatal(err)
	}
	policies := access.New(db, a)
	collection, err := policies.CreateLibrary(t.Context(), owner, "Inventory", true, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	root := walkRoot(t)
	sources := library.NewSources(db, a, queue, map[string]media.SourceRoot{"fixture": root})
	settings := library.DefaultSourceSettings()
	settings.LibraryID, settings.RootName = collection.ID, "fixture"
	source, err := sources.Create(t.Context(), owner, settings, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	policy := DefaultPolicy()
	policy.BatchSize = 2
	inventory, err := NewInventory(db, queue, sources, policy)
	if err != nil {
		t.Fatal(err)
	}
	return inventoryFixture{db, queue, sources, inventory, owner, a, source, root.Path}
}

func (f inventoryFixture) claim(t *testing.T, dedupe string) jobs.Lease {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, created, err := f.queue.EnqueueTx(t.Context(), tx, jobs.Input{Kind: "scan", ResourceKey: "source:" + f.source.ID, Dedupe: dedupe, Payload: json.RawMessage(`{}`), ActorID: f.owner.User.ID})
	if err != nil || !created {
		t.Fatal("enqueue", created, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	lease, err := f.queue.Claim(t.Context(), store.NewID(), []string{"scan"})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func (f inventoryFixture) files(t *testing.T) []File {
	t.Helper()
	var files []File
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	if _, err := Walk(t.Context(), root, f.inventory.policy, func(file File) error { files = append(files, file); return nil }); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestBuildSealsCompleteInventoryWithoutPublishingIndexedData(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "One/photo.png", []byte("first"))
	writeInput(t, root, "Two/photo.png", []byte("second"))
	writeInput(t, root, "root-file", nil)
	lease := f.claim(t, "scan:build")
	generation, err := f.inventory.Build(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "incremental", "One")
	if err != nil || generation.State != "ready" || !generation.Healthy || generation.FileCount != 3 || generation.Number != 1 || generation.CompletedAt == nil {
		t.Fatal(generation, err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "running" || job.Done != 3 || job.Succeeded != 3 || job.TotalKnown {
		t.Fatal("staging claimed indexing or final progress", job, err)
	}
	current, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil || current.ScanGeneration != 0 {
		t.Fatal("staging published source generation", current, err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM albums)+(SELECT COUNT(*) FROM media)+(SELECT COUNT(*) FROM source_entries)`).Scan(&count); err != nil || count != 0 {
		t.Fatal("staging mutated indexed data", count, err)
	}
	encoded, _ := json.Marshal(generation)
	for _, secret := range []string{lease.Owner, "lease_owner", "base_counts_json", f.rootPath} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("private generation field leaked", secret)
		}
	}
	_, err = f.inventory.Record(t.Context(), lease, generation.ID, f.files(t)[:1])
	walkError(t, err, ErrGeneration)
}

func TestInventoryRecordIsIdempotentAndProgressOutboxFailureRollsBack(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "one", []byte("one"))
	writeInput(t, root, "two", []byte("two"))
	lease := f.claim(t, "scan:atomic")
	generation, err := f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil {
		t.Fatal(err)
	}
	files := f.files(t)
	if _, err := f.db.Exec(`CREATE TRIGGER fail_inventory_progress BEFORE INSERT ON outbox_events WHEN (SELECT kind FROM change_log WHERE seq=NEW.change_seq)='job' BEGIN SELECT RAISE(ABORT,'injected late progress failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.Record(t.Context(), lease, generation.ID, files); err == nil {
		t.Fatal("staging committed without job progress event")
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inventory_files`).Scan(&count); err != nil || count != 0 {
		t.Fatal("staged rows survived progress rollback", count, err)
	}
	current, err := f.inventory.Get(t.Context(), generation.ID)
	if err != nil || current.FileCount != 0 || current.Version != generation.Version {
		t.Fatal("partial generation progress", current, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_inventory_progress`); err != nil {
		t.Fatal(err)
	}
	stored, err := f.inventory.Record(t.Context(), lease, generation.ID, files)
	if err != nil || stored.FileCount != 2 {
		t.Fatal(stored, err)
	}
	replay, err := f.inventory.Record(t.Context(), lease, generation.ID, files)
	if err != nil || replay.Version != stored.Version || replay.FileCount != 2 {
		t.Fatal("staging replay duplicated work", replay, err)
	}
	changed := files[0]
	changed.SHA256 = strings.Repeat("0", 64)
	_, err = f.inventory.Record(t.Context(), lease, generation.ID, []File{changed})
	walkError(t, err, ErrChanged)
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Succeeded != 2 || job.Done != 2 {
		t.Fatal("staging replay doubled job counters", job, err)
	}
	for _, file := range []File{{RelativePath: "../outside"}, {RelativePath: "safe", SHA256: strings.Repeat("z", 64)}} {
		_, err := f.inventory.Record(t.Context(), lease, generation.ID, []File{file})
		walkError(t, err, ErrUnsafeEntry)
	}
}

func TestActiveGenerationIsExclusiveAndExpiredOwnerCannotStage(t *testing.T) {
	f := inventorySetup(t)
	first := f.claim(t, "scan:first")
	second := f.claim(t, "scan:competing")
	leases := []jobs.Lease{first, second}
	var wg sync.WaitGroup
	results := make(chan struct {
		generation Generation
		err        error
		index      int
	}, 2)
	for index, lease := range leases {
		wg.Go(func() {
			generation, err := f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
			results <- struct {
				generation Generation
				err        error
				index      int
			}{generation, err, index}
		})
	}
	wg.Wait()
	close(results)
	var active Generation
	owner := 0
	success := 0
	for result := range results {
		if result.err == nil {
			active = result.generation
			owner = result.index
			success++
		} else {
			walkError(t, result.err, ErrActiveGeneration)
		}
	}
	if success != 1 {
		t.Fatal("source acquired multiple active generations", success)
	}
	if _, err := f.db.Exec(`UPDATE jobs SET lease_expires_at=0 WHERE id=?`, leases[owner].Job.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.inventory.Begin(t.Context(), leases[1-owner], f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil || replacement.Number != 2 {
		t.Fatal(replacement, err)
	}
	old, err := f.inventory.Get(t.Context(), active.ID)
	if err != nil || old.State != "superseded" || old.Healthy {
		t.Fatal("expired generation remained authoritative", old, err)
	}
	_, err = f.inventory.Record(t.Context(), leases[owner], active.ID, []File{{}})
	walkError(t, err, jobs.ErrLeaseLost)
	_, err = f.inventory.Seal(t.Context(), leases[owner], active.ID)
	walkError(t, err, jobs.ErrLeaseLost)
}

func TestSealDetectsEqualStatOverwriteAndRecordsSafeFailure(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	data := bytes.Repeat([]byte("a"), hashChunk*3)
	name := writeInput(t, root, "changing.bin", data)
	lease := f.claim(t, "scan:changed")
	generation, err := f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil {
		t.Fatal(err)
	}
	files := f.files(t)
	if _, err := f.inventory.Record(t.Context(), lease, generation.ID, files); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	data[hashChunk+10] = 'b'
	if err := os.Chmod(name, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	_, err = f.inventory.Seal(t.Context(), lease, generation.ID)
	walkError(t, err, ErrChanged)
	if err := f.inventory.Abort(t.Context(), lease, generation.ID, "SOURCE_CHANGED", "changing.bin"); err != nil {
		t.Fatal(err)
	}
	failed, err := f.inventory.Get(t.Context(), generation.ID)
	if err != nil || failed.State != "failed" || failed.Healthy || failed.ErrorCode == nil || *failed.ErrorCode != "SOURCE_CHANGED" {
		t.Fatal(failed, err)
	}
	var issue string
	if err := f.db.QueryRow(`SELECT relative_path FROM inventory_issues WHERE generation_id=?`, generation.ID).Scan(&issue); err != nil || issue != "changing.bin" {
		t.Fatal("per-path diagnostic missing", issue, err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Succeeded != 1 || job.Failed != 1 || job.Done != 2 {
		t.Fatal("failed verification invented success", job, err)
	}
}

func TestMinimumGuardMountLossAndConfigurationCancellationNeverPublish(t *testing.T) {
	f := inventorySetup(t)
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,display_name,created_at,updated_at) VALUES('preserved',?,?,'Source title','User override',0,0)`, f.source.LibraryID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,updated_at) VALUES('preserved-entry',?,'one',3,0,0)`, f.source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorites(user_id,album_id,created_at) VALUES(?,'preserved',0)`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "one", []byte("one"))
	minimum := int64(2)
	updated, err := f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{MinimumFiles: &minimum})
	if err != nil {
		t.Fatal(err)
	}
	f.source = updated
	lease := f.claim(t, "scan:minimum")
	generation, err := f.inventory.Begin(t.Context(), lease, f.source.ID, f.source.ConfigVersion, "full", ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.Record(t.Context(), lease, generation.ID, f.files(t)); err != nil {
		t.Fatal(err)
	}
	_, err = f.inventory.Seal(t.Context(), lease, generation.ID)
	walkError(t, err, ErrMinimumFiles)
	if err := os.Remove(filepath.Join(root.Path, root.Marker)); err != nil {
		t.Fatal(err)
	}
	_, err = f.inventory.Seal(t.Context(), lease, generation.ID)
	if err == nil {
		t.Fatal("missing mount marker blessed inventory")
	}
	if _, err := os.Stat(filepath.Join(root.Path, root.Marker)); !os.IsNotExist(err) {
		t.Fatal("missing marker recreated", err)
	}
	current, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil || current.ScanGeneration != 0 {
		t.Fatal("failed generation changed published source", current, err)
	}
	_, err = f.sources.Update(t.Context(), f.owner, f.source.ID, current.Version, library.SourcePatch{MinimumFiles: sourceInt64(1)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.inventory.Record(t.Context(), lease, generation.ID, fakeFiles())
	walkError(t, err, jobs.ErrCancelled)
	_, err = f.inventory.Seal(t.Context(), lease, generation.ID)
	walkError(t, err, jobs.ErrCancelled)
	var retained int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM favorites f JOIN albums a ON a.id=f.album_id JOIN source_entries e ON e.source_id=a.source_id WHERE a.id='preserved' AND a.display_name='User override' AND e.availability='present'`).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("failed/incomplete inventory changed the old index or interaction", retained, err)
	}
}

func sourceInt64(value int64) *int64 { return &value }
func fakeFiles() []File {
	return []File{{RelativePath: "one", SizeBytes: 3, Identity: "1:1", QuickFingerprint: strings.Repeat("a", 64), SHA256: strings.Repeat("a", 64)}}
}
