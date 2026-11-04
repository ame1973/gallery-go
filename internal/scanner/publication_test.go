package scanner

import (
	"bytes"
	"database/sql"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func publicationPNG(t *testing.T, width int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, width, 7))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func (f inventoryFixture) preparePublication(t *testing.T, mode, scope string) (jobs.Lease, Generation) {
	t.Helper()
	lease := f.claim(t, "scan:"+store.NewID())
	generation, err := f.inventory.Build(t.Context(), lease, f.source.ID, f.source.ConfigVersion, mode, scope)
	if err != nil {
		t.Fatal(err)
	}
	generation, err = f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, media.DefaultInspectPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return lease, generation
}
func (f inventoryFixture) publish(t *testing.T, mode, scope string) Generation {
	t.Helper()
	lease, generation := f.preparePublication(t, mode, scope)
	published, err := f.inventory.Publish(t.Context(), lease, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if published.State != "published" || published.PublishedAt == nil {
		t.Fatal("generation not published", published)
	}
	return published
}

func TestPublicationCreatesOwnedRevisionsAndRealAuthorizedOriginals(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "Album/photo.png", publicationPNG(t, 12))
	writeInput(t, root, "Album/corrupt.jpg", []byte("invalid"))
	writeInput(t, root, "notes.txt", []byte("not media"))
	generation := f.publish(t, "full", ".")
	var entries, albums, medias, revisions, variants int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM source_entries),(SELECT COUNT(*) FROM albums),(SELECT COUNT(*) FROM media),(SELECT COUNT(*) FROM media_revisions),(SELECT COUNT(*) FROM media_variants)`).Scan(&entries, &albums, &medias, &revisions, &variants); err != nil || entries != 3 || albums != 1 || medias != 2 || revisions != 2 || variants != 0 {
		t.Fatal("incorrect indexed publication", entries, albums, medias, revisions, variants, err)
	}
	var id, entry, revision string
	var width, height int64
	if err := f.db.QueryRow(`SELECT id,source_entry_id,current_revision_id,width,height FROM media WHERE file_name='photo.png'`).Scan(&id, &entry, &revision, &width, &height); err != nil || width != 12 || height != 7 {
		t.Fatal(id, width, height, err)
	}
	var owner, hash string
	if err := f.db.QueryRow(`SELECT media_id,source_sha256 FROM media_revisions WHERE id=?`, revision).Scan(&owner, &hash); err != nil || owner != id {
		t.Fatal("revision owner", owner, err)
	}
	if _, err := f.db.Exec(`UPDATE users SET download_original=1 WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	content, err := service.OpenOriginal(t.Context(), f.owner, id)
	if err != nil {
		t.Fatal("published original unavailable", err)
	}
	content.Close()
	job, err := f.queue.Get(t.Context(), generation.JobID)
	if err != nil || job.Status != "failed" || job.Failed != 1 || !job.TotalKnown || job.Done != job.Total {
		t.Fatal("per-media error became job success", job, err)
	}
	current, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil || current.State != "healthy" || current.ScanGeneration != generation.Number || current.LastFullScanAt == nil {
		t.Fatal(current, err)
	}
	var orphanEvents int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM change_log c WHERE NOT EXISTS(SELECT 1 FROM outbox_events o WHERE o.change_seq=c.seq)`).Scan(&orphanEvents); err != nil || orphanEvents != 0 {
		t.Fatal("event missing outbox", orphanEvents, err)
	}
	var beforeVersion, beforeAlbumVersion int64
	if err := f.db.QueryRow(`SELECT m.version,a.version FROM media m JOIN albums a ON a.id=m.album_id WHERE m.id=?`, id).Scan(&beforeVersion, &beforeAlbumVersion); err != nil {
		t.Fatal(err)
	}
	repeated := f.publish(t, "incremental", ".")
	var nextID, nextEntry, nextRevision string
	var nextVersion, nextAlbumVersion int64
	if err := f.db.QueryRow(`SELECT m.id,m.source_entry_id,m.current_revision_id,m.version,a.version FROM media m JOIN albums a ON a.id=m.album_id WHERE m.file_name='photo.png'`).Scan(&nextID, &nextEntry, &nextRevision, &nextVersion, &nextAlbumVersion); err != nil || nextID != id || nextEntry != entry || nextRevision != revision || nextVersion != beforeVersion || nextAlbumVersion != beforeAlbumVersion {
		t.Fatal("identical rescan changed identity/version", nextID, nextEntry, nextRevision, nextVersion, nextAlbumVersion, err)
	}
	var seen int64
	if err := f.db.QueryRow(`SELECT seen_generation FROM source_entries WHERE id=?`, entry).Scan(&seen); err != nil || seen != repeated.Number {
		t.Fatal("private generation evidence not refreshed", seen, err)
	}
}

func TestPublicationNonMediaReplacementAndArchiveFailClosedForContent(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	name := writeInput(t, root, "Album/no-extension", publicationPNG(t, 12))
	f.publish(t, "full", ".")
	var id, album string
	if err := f.db.QueryRow(`SELECT id,album_id FROM media`).Scan(&id, &album); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE users SET download_original=1 WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte("not media anymore"), 0o400); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	var availability, status string
	var width sql.NullInt64
	if err := f.db.QueryRow(`SELECT availability,json_extract(metadata_json,'$.status'),width FROM media WHERE id=?`, id).Scan(&availability, &status, &width); err != nil || availability != "unavailable" || status != "not_media" || width.Valid {
		t.Fatal("non-media retained fabricated metadata", availability, status, width, err)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	_, err := service.OpenOriginal(t.Context(), f.owner, id)
	walkError(t, err, media.ErrSourceUnavailable)
	if _, err := f.db.Exec(`UPDATE media SET availability='archived' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	_, err = service.Get(t.Context(), f.owner, id)
	walkError(t, err, sql.ErrNoRows)
	_, err = service.OpenOriginal(t.Context(), f.owner, id)
	walkError(t, err, sql.ErrNoRows)
	items, err := service.AlbumMedia(t.Context(), f.owner, album, "", 30)
	if err != nil || len(items) != 0 {
		t.Fatal("archived media escaped shared visibility", items, err)
	}
}

func TestVerifiedPublicationRejectsNewerSourceHealthEvidence(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "photo.png", publicationPNG(t, 12))
	lease, generation := f.preparePublication(t, "full", ".")
	source, err := f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.inventory.verifyManifest(t.Context(), generation, source, root); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sources.Check(t.Context(), f.owner, source.ID, source.Version, store.NewID()); err != nil {
		t.Fatal(err)
	}
	_, err = f.inventory.publishVerified(t.Context(), lease, generation, source.Version, true)
	walkError(t, err, library.ErrSourceChanged)
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM media`).Scan(&count); err != nil || count != 0 {
		t.Fatal("stale source-version proof published", count, err)
	}
	if _, err := f.inventory.Publish(t.Context(), lease, generation.ID); err != nil {
		t.Fatal("fresh source/version validation did not permit retry", err)
	}
}

func TestPublicationRechecksLeaseAfterAllBusinessWritesForEveryPart(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(map[bool]string{false: "part", true: "final"}[final], func(t *testing.T) {
			f := inventorySetup(t)
			writeInput(t, media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}, "photo.png", publicationPNG(t, 12))
			lease, generation := f.preparePublication(t, "full", ".")
			if _, err := f.db.Exec(`CREATE TRIGGER expire_publication AFTER INSERT ON audit_events WHEN NEW.event_type='inventory_published' BEGIN UPDATE jobs SET lease_expires_at=0 WHERE id IN (SELECT job_id FROM inventory_generations WHERE id=NEW.resource_id); END`); err != nil {
				t.Fatal(err)
			}
			var err error
			if final {
				_, err = f.inventory.Publish(t.Context(), lease, generation.ID)
			} else {
				_, err = f.inventory.PublishPart(t.Context(), lease, generation.ID)
			}
			walkError(t, err, jobs.ErrLeaseLost)
			var count int
			if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media)+(SELECT COUNT(*) FROM source_entries)+(SELECT scan_generation FROM sources)`).Scan(&count); err != nil || count != 0 {
				t.Fatal("late lost lease committed business state", count, err)
			}
			if _, err := f.db.Exec(`DROP TRIGGER expire_publication`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.inventory.Publish(t.Context(), lease, generation.ID); err != nil {
				t.Fatal("late lease rollback prevented retry", err)
			}
		})
	}
}

func TestPublicationEmptySourceCompletesWithoutInventingMedia(t *testing.T) {
	f := inventorySetup(t)
	generation := f.publish(t, "full", ".")
	job, err := f.queue.Get(t.Context(), generation.JobID)
	if err != nil || job.Status != "succeeded" || job.Done != 0 || !job.TotalKnown {
		t.Fatal(job, err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM media`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestPublicationSamePathOverwriteCreatesImmutableOwnedRevision(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	name := writeInput(t, root, "Album/photo.png", publicationPNG(t, 12))
	f.publish(t, "full", ".")
	var id, oldRevision string
	var oldVersion int64
	if err := f.db.QueryRow(`SELECT id,current_revision_id,version FROM media`).Scan(&id, &oldRevision, &oldVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO media_variants(id,revision_id,preset_key,preset_version,storage_path,mime,size_bytes,sha256,status,created_at) VALUES('old-variant',?,'thumb_320',1,'never-public','image/png',1,?,'ready',0)`, oldRevision, hashForTest()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, publicationPNG(t, 13), 0o400); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "incremental", ".")
	var currentID, currentRevision string
	var version, width int64
	if err := f.db.QueryRow(`SELECT id,current_revision_id,version,width FROM media`).Scan(&currentID, &currentRevision, &version, &width); err != nil || currentID != id || currentRevision == oldRevision || version != oldVersion+1 || width != 13 {
		t.Fatal("overwrite replaced logical identity or old revision", currentID, currentRevision, version, width, err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM media_revisions WHERE media_id=?`, id).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	_, err := service.OpenVariant(t.Context(), f.owner, id, "old-variant")
	walkError(t, err, sql.ErrNoRows)
}

func hashForTest() string { return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" }

func TestPublicationRevalidatesAfterMetadataBeforeIndexing(t *testing.T) {
	for _, change := range []string{"add", "delete", "equal-stat", "marker"} {
		t.Run(change, func(t *testing.T) {
			f := inventorySetup(t)
			root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
			data := append(publicationPNG(t, 12), bytes.Repeat([]byte("a"), hashChunk*2)...)
			name := writeInput(t, root, "photo.png", data)
			lease, generation := f.preparePublication(t, "full", ".")
			switch change {
			case "add":
				writeInput(t, root, "new.png", publicationPNG(t, 13))
			case "delete":
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "marker":
				if err := os.Remove(filepath.Join(root.Path, root.Marker)); err != nil {
					t.Fatal(err)
				}
			case "equal-stat":
				info, err := os.Stat(name)
				if err != nil {
					t.Fatal(err)
				}
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
			}
			if _, err := f.inventory.Publish(t.Context(), lease, generation.ID); err == nil {
				t.Fatal("stale ready metadata published", change)
			}
			var count int
			if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media)+(SELECT COUNT(*) FROM albums)+(SELECT COUNT(*) FROM source_entries)+(SELECT scan_generation FROM sources)`).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed validation changed index", count, err)
			}
		})
	}
}
