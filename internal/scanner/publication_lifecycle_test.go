package scanner

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func TestPublicationCreatorOverridesKeepIndexedIdentityAcrossFolderMoves(t *testing.T) {
	f := inventorySetup(t)
	mapping := library.Mapping{AlbumDepth: 2, PersonDepth: 1}
	var err error
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{Mapping: &mapping})
	if err != nil {
		t.Fatal(err)
	}
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "Mapped/Album/one.png", publicationPNG(t, 21))
	writeInput(t, root, "Manual/Other/two.png", publicationPNG(t, 22))
	f.publish(t, "full", ".")
	var album, mapped, selected string
	var version int64
	if err := f.db.QueryRow(`SELECT id,mapped_person_id,version FROM albums WHERE source_id=? AND relative_path='Mapped/Album'`, f.source.ID).Scan(&album, &mapped, &version); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT id FROM people WHERE source_id=? AND relative_path='Manual'`, f.source.ID).Scan(&selected); err != nil {
		t.Fatal(err)
	}
	service := access.New(f.db, f.auth)
	changed, err := service.SetAlbumCreator(t.Context(), f.owner, album, version, access.AlbumCreatorRequest{Mode: "person", PersonID: &selected}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	stable, err := service.AdminAlbumCreator(t.Context(), f.owner, album)
	if err != nil || stable.Version != changed.Version || stable.Mode != "person" || stable.PersonID == nil || *stable.PersonID != selected {
		t.Fatal("no-op scan overwrote a manual creator", stable, changed, err)
	}
	if err := os.Rename(filepath.Join(root.Path, "Mapped"), filepath.Join(root.Path, "Renamed")); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	current, err := service.AdminAlbumCreator(t.Context(), f.owner, album)
	if err != nil || current.AlbumID != album || current.PersonID == nil || *current.PersonID != selected || current.MappedPersonID == nil || *current.MappedPersonID != mapped || current.OverridePersonID == nil || *current.OverridePersonID != selected || current.Version <= changed.Version {
		t.Fatal("folder move confused manual creator with indexed provenance", current, changed, err)
	}
	var mappedName, mappedPath, selectedName, selectedPath string
	if err := f.db.QueryRow(`SELECT source_name,relative_path FROM people WHERE id=?`, mapped).Scan(&mappedName, &mappedPath); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT source_name,relative_path FROM people WHERE id=?`, selected).Scan(&selectedName, &selectedPath); err != nil {
		t.Fatal(err)
	}
	if mappedName != "Renamed" || mappedPath != "Renamed" || selectedName != "Manual" || selectedPath != "Manual" {
		t.Fatal("rename stole or merged a manually selected creator identity", mappedName, mappedPath, selectedName, selectedPath)
	}
	none, err := service.SetAlbumCreator(t.Context(), f.owner, album, current.Version, access.AlbumCreatorRequest{Mode: "none"}, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root.Path, "Renamed"), filepath.Join(root.Path, "Final")); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	current, err = service.AdminAlbumCreator(t.Context(), f.owner, album)
	if err != nil || current.Mode != "none" || current.PersonID != nil || current.MappedPersonID == nil || *current.MappedPersonID != mapped || current.Version <= none.Version {
		t.Fatal("scan restored an explicitly unclassified creator", current, none, err)
	}
	restored, err := service.SetAlbumCreator(t.Context(), f.owner, album, current.Version, access.AlbumCreatorRequest{Mode: "mapped"}, store.NewID())
	if err != nil || restored.PersonID == nil || *restored.PersonID != mapped {
		t.Fatal("restore mapped mode lost the latest indexed identity", restored, err)
	}
	person, err := service.Person(t.Context(), f.owner, mapped)
	if err != nil || person.Name != "Final" {
		t.Fatal("mapped fallback restored an old folder label", person, err)
	}
	mapping.PersonDepth = 0
	if err := f.db.QueryRow(`SELECT version FROM sources WHERE id=?`, f.source.ID).Scan(&f.source.Version); err != nil {
		t.Fatal(err)
	}
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{Mapping: &mapping})
	if err != nil {
		t.Fatal(err)
	}
	var scope string
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.owner.User.ID).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	current, err = service.AdminAlbumCreator(t.Context(), f.owner, album)
	if err != nil || current.Mode != "mapped" || current.PersonID != nil || current.MappedPersonID != nil {
		t.Fatal("new source mapping did not replace only the indexed fallback", current, err)
	}
	var after string
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.owner.User.ID).Scan(&after); err != nil || after == scope {
		t.Fatal("scan classification loss did not revoke old creator scope", scope, after, err)
	}
}

func TestPublicationWholeFolderRenamePreservesAlbumsPeopleAndInteractions(t *testing.T) {
	f := inventorySetup(t)
	mapping := library.Mapping{AlbumDepth: 2, PersonDepth: 1}
	var err error
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{Mapping: &mapping})
	if err != nil {
		t.Fatal(err)
	}
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "Alice/Trip/one.png", publicationPNG(t, 12))
	writeInput(t, root, "Alice/Trip/two.png", publicationPNG(t, 13))
	f.publish(t, "full", ".")
	var album, person, cover string
	if err := f.db.QueryRow(`SELECT id,person_id,cover_media_id FROM albums`).Scan(&album, &person, &cover); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE albums SET display_name='Album override',access_restricted=1,cover_override_id=cover_media_id WHERE id=?`, []any{album}},
		{`UPDATE people SET display_name='Person override' WHERE id=?`, []any{person}},
		{`INSERT INTO favorites(user_id,album_id,created_at) VALUES(?,?,0)`, []any{f.owner.User.ID, album}},
		{`INSERT INTO ratings(user_id,album_id,rating,updated_at) VALUES(?,?,5,0)`, []any{f.owner.User.ID, album}},
		{`INSERT INTO tags(id,name) VALUES('tag','Retained')`, nil},
		{`INSERT INTO album_tags VALUES(?,'tag')`, []any{album}},
	} {
		if _, err := f.db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string][2]string{}
	rows, err := f.db.Query(`SELECT m.id,m.source_entry_id,m.current_revision_id FROM media m`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, entry, revision string
		if err := rows.Scan(&id, &entry, &revision); err != nil {
			t.Fatal(err)
		}
		before[id] = [2]string{entry, revision}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if err := os.Rename(filepath.Join(root.Path, "Alice"), filepath.Join(root.Path, "Bob")); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "incremental", ".")
	var gotAlbum, gotPerson, gotCover, albumTitle, personTitle, personPath, albumPath string
	var restricted bool
	if err := f.db.QueryRow(`SELECT a.id,a.person_id,a.cover_media_id,a.display_name,p.display_name,p.relative_path,a.relative_path,a.access_restricted FROM albums a JOIN people p ON p.id=a.person_id`).Scan(&gotAlbum, &gotPerson, &gotCover, &albumTitle, &personTitle, &personPath, &albumPath, &restricted); err != nil || gotAlbum != album || gotPerson != person || gotCover != cover || albumTitle != "Album override" || personTitle != "Person override" || personPath != "Bob" || albumPath != "Bob/Trip" || !restricted {
		t.Fatal("folder rename discarded identity/overrides/restriction", gotAlbum, gotPerson, gotCover, albumTitle, personTitle, personPath, albumPath, restricted, err)
	}
	for id, previous := range before {
		var entry, revision string
		if err := f.db.QueryRow(`SELECT source_entry_id,current_revision_id FROM media WHERE id=?`, id).Scan(&entry, &revision); err != nil || entry != previous[0] || revision != previous[1] {
			t.Fatal("rename replaced stable logical/revision identity", id, entry, revision, err)
		}
	}
	var retained int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM favorites WHERE album_id=?)+(SELECT COUNT(*) FROM ratings WHERE album_id=?)+(SELECT COUNT(*) FROM album_tags WHERE album_id=?)`, album, album, album).Scan(&retained); err != nil || retained != 3 {
		t.Fatal("interactions lost", retained, err)
	}
	var data string
	if err := f.db.QueryRow(`SELECT payload_json FROM change_log WHERE kind='album' AND resource_id=? ORDER BY seq DESC LIMIT 1`, album).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var event struct {
		TagIDs []string `json:"tag_ids"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil || len(event.TagIDs) != 1 || event.TagIDs[0] != "tag" {
		t.Fatal("scan/move album upsert lost the retained canonical tag set", data, err)
	}
}

func TestPublicationFrozenRenameAmbiguityNeverGuessesLastCandidate(t *testing.T) {
	for _, shape := range []struct {
		name     string
		old, new int
	}{{"one-old-two-new", 1, 2}, {"two-old-one-new", 2, 1}} {
		t.Run(shape.name, func(t *testing.T) {
			f := inventorySetup(t)
			root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
			data := publicationPNG(t, 12)
			for i := range shape.old {
				writeInput(t, root, filepath.ToSlash(filepath.Join("Old", string(rune('a'+i))+".png")), data)
			}
			f.publish(t, "full", ".")
			var oldAlbum string
			if err := f.db.QueryRow(`SELECT id FROM albums`).Scan(&oldAlbum); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`INSERT INTO favorites(user_id,album_id,created_at) VALUES(?,?,0)`, f.owner.User.ID, oldAlbum); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(root.Path, "Old")); err != nil {
				t.Fatal(err)
			}
			for i := range shape.new {
				writeInput(t, root, filepath.ToSlash(filepath.Join("New", string(rune('a'+i))+".png")), data)
			}
			generation := f.publish(t, "full", ".")
			var oldMissing, newPresent, mediaCount, issues, retained int
			if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM source_entries WHERE relative_path GLOB 'Old/*' AND availability='unavailable'),(SELECT COUNT(*) FROM source_entries WHERE relative_path GLOB 'New/*' AND availability='present'),(SELECT COUNT(*) FROM media),(SELECT COUNT(*) FROM inventory_issues WHERE generation_id=? AND error_code='AMBIGUOUS_RENAME'),(SELECT COUNT(*) FROM favorites WHERE album_id=?)`, generation.ID, oldAlbum).Scan(&oldMissing, &newPresent, &mediaCount, &issues, &retained); err != nil || oldMissing != shape.old || newPresent != shape.new || mediaCount != shape.old+shape.new || issues != shape.new || retained != 1 {
				t.Fatal("ambiguous rename was guessed or metadata purged", oldMissing, newPresent, mediaCount, issues, retained, err)
			}
			job, err := f.queue.Get(t.Context(), generation.JobID)
			if err != nil || job.Status != "failed" || job.Failed != int64(shape.new) {
				t.Fatal("ambiguity was hidden in counters", job, err)
			}
		})
	}
}

func TestPublicationMissingRequiresHealthyFullEvidenceAndNeverPurges(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	data := publicationPNG(t, 12)
	name := writeInput(t, root, "Album/photo.png", data)
	f.publish(t, "full", ".")
	var mediaID, albumID, entryID string
	if err := f.db.QueryRow(`SELECT id,album_id,source_entry_id FROM media`).Scan(&mediaID, &albumID, &entryID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorites(user_id,album_id,created_at) VALUES(?,?,0)`, f.owner.User.ID, albumID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "incremental", ".")
	var availability string
	var full int64
	if err := f.db.QueryRow(`SELECT availability,missing_full_scans FROM source_entries WHERE id=?`, entryID).Scan(&availability, &full); err != nil || availability != "unavailable" || full != 0 {
		t.Fatal("incremental absence invented full evidence", availability, full, err)
	}
	if _, err := f.db.Exec(`UPDATE source_entries SET missing_since=? WHERE id=?`, time.Now().Add(-25*time.Hour).UnixMilli(), entryID); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	if err := f.db.QueryRow(`SELECT availability,missing_full_scans FROM source_entries WHERE id=?`, entryID).Scan(&availability, &full); err != nil || availability != "unavailable" || full != 1 {
		t.Fatal("single healthy full snapshot quarantined too early", availability, full, err)
	}
	f.publish(t, "full", ".")
	var mediaAvailability string
	var quarantine sql.NullInt64
	var retained int
	if err := f.db.QueryRow(`SELECT e.availability,m.availability,a.quarantined_at,(SELECT COUNT(*) FROM favorites WHERE album_id=a.id) FROM source_entries e JOIN media m ON m.source_entry_id=e.id JOIN albums a ON a.id=m.album_id WHERE e.id=?`, entryID).Scan(&availability, &mediaAvailability, &quarantine, &retained); err != nil || availability != "quarantined" || mediaAvailability != "quarantined" || !quarantine.Valid || retained != 1 {
		t.Fatal("healthy quarantine deleted or invented data", availability, mediaAvailability, quarantine, retained, err)
	}
	writeInput(t, root, "Album/photo.png", data)
	f.publish(t, "full", ".")
	var currentMedia, currentEntry string
	var since sql.NullInt64
	if err := f.db.QueryRow(`SELECT m.id,e.id,e.availability,e.missing_since,e.missing_full_scans,a.quarantined_at FROM media m JOIN source_entries e ON e.id=m.source_entry_id JOIN albums a ON a.id=m.album_id`).Scan(&currentMedia, &currentEntry, &availability, &since, &full, &quarantine); err != nil || currentMedia != mediaID || currentEntry != entryID || availability != "present" || since.Valid || full != 0 || quarantine.Valid {
		t.Fatal("returning source lost identity or missing state did not clear", currentMedia, currentEntry, availability, since, full, quarantine, err)
	}
}

func TestPublicationTargetDefersCrossScopeRenameAndMissingChanges(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	a := writeInput(t, root, "A/a.png", publicationPNG(t, 12))
	b := writeInput(t, root, "B/b.png", publicationPNG(t, 13))
	first := f.publish(t, "full", ".")
	var originalMedia string
	if err := f.db.QueryRow(`SELECT id FROM media WHERE file_name='a.png'`).Scan(&originalMedia); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "incremental", "A")
	var availability string
	var seen int64
	if err := f.db.QueryRow(`SELECT availability,seen_generation FROM source_entries WHERE relative_path='B/b.png'`).Scan(&availability, &seen); err != nil || availability != "present" || seen != first.Number {
		t.Fatal("target reconciled outside scope", availability, seen, err)
	}
	writeInput(t, root, "C/placeholder.txt", []byte("not media"))
	if err := os.Rename(a, filepath.Join(root.Path, "C", "a.png")); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"A", "C"} {
		generation := f.publish(t, "incremental", scope)
		var deferred, count int
		if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM inventory_issues WHERE generation_id=? AND error_code='TARGET_RENAME_OUTSIDE_SCOPE'),(SELECT COUNT(*) FROM source_entries WHERE relative_path='C/a.png')`, generation.ID).Scan(&deferred, &count); err != nil || deferred != 1 || count != 0 {
			t.Fatal("target failed to defer an incoming or outgoing cross-scope identity", scope, deferred, count, err)
		}
		job, err := f.queue.Get(t.Context(), generation.JobID)
		if err != nil || job.Status != "failed" || job.Failed != 1 {
			t.Fatal("cross-scope target falsely reported success", scope, job, err)
		}
	}
	f.publish(t, "full", ".")
	var currentMedia string
	if err := f.db.QueryRow(`SELECT m.id FROM media m JOIN source_entries e ON e.id=m.source_entry_id WHERE e.relative_path='C/a.png'`).Scan(&currentMedia); err != nil || currentMedia != originalMedia {
		t.Fatal("deferred full rename lost identity", currentMedia, err)
	}
}

func TestPublicationRollbackIncludesBusinessScopesProgressAndTerminalJob(t *testing.T) {
	for _, failure := range []string{"source-outbox", "audit", "job-completion"} {
		t.Run(failure, func(t *testing.T) {
			f := inventorySetup(t)
			writeInput(t, media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}, "Album/photo.png", publicationPNG(t, 12))
			lease, generation := f.preparePublication(t, "full", ".")
			trigger := `CREATE TRIGGER fail_publication BEFORE INSERT ON outbox_events WHEN (SELECT kind FROM change_log WHERE seq=NEW.change_seq)='source' BEGIN SELECT RAISE(ABORT,'injected source outbox failure'); END`
			if failure == "audit" {
				trigger = `CREATE TRIGGER fail_publication BEFORE INSERT ON audit_events WHEN NEW.event_type='inventory_published' BEGIN SELECT RAISE(ABORT,'injected publication audit failure'); END`
			}
			if failure == "job-completion" {
				trigger = `CREATE TRIGGER fail_publication BEFORE INSERT ON outbox_events WHEN EXISTS(SELECT 1 FROM change_log c WHERE c.seq=NEW.change_seq AND c.kind='job' AND json_extract(c.payload_json,'$.status')='succeeded') BEGIN SELECT RAISE(ABORT,'injected final job failure'); END`
			}
			if _, err := f.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := f.inventory.Publish(t.Context(), lease, generation.ID); err == nil {
				t.Fatal("late failure did not rollback publication", failure)
			}
			var count int
			var scan int64
			if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media)+(SELECT COUNT(*) FROM media_revisions)+(SELECT COUNT(*) FROM albums)+(SELECT COUNT(*) FROM source_entries),(SELECT scan_generation FROM sources)`).Scan(&count, &scan); err != nil || count != 0 || scan != 0 {
				t.Fatal("partial indexed publication", count, scan, err)
			}
			current, err := f.inventory.Get(t.Context(), generation.ID)
			if err != nil || current.State != "ready" || current.PublishedAt != nil || current.Version != generation.Version {
				t.Fatal("generation escaped rollback", current, err)
			}
			job, err := f.queue.Get(t.Context(), lease.Job.ID)
			if err != nil || job.Status != "running" || job.Done != 2 || job.TotalKnown {
				t.Fatal("progress/completion escaped rollback", job, err)
			}
			if _, err := f.db.Exec(`DROP TRIGGER fail_publication`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.inventory.Publish(t.Context(), lease, generation.ID); err != nil {
				t.Fatal("rollback left connection-local planning state or blocked retry", err)
			}
		})
	}
}

func TestPublicationPartsKeepMultiSourceJobRunningUntilFinalSource(t *testing.T) {
	f := inventorySetup(t)
	firstRoot := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	otherRoot := walkRoot(t)
	f.sources = library.NewSources(f.db, f.auth, f.queue, map[string]media.SourceRoot{"fixture": firstRoot, "other": otherRoot})
	f.inventory.sources = f.sources
	settings := library.DefaultSourceSettings()
	settings.LibraryID = f.source.LibraryID
	settings.RootName = "other"
	other, err := f.sources.Create(t.Context(), f.owner, settings, "multi-source-registration")
	if err != nil {
		t.Fatal(err)
	}
	writeInput(t, firstRoot, "photo.png", publicationPNG(t, 12))
	writeInput(t, otherRoot, "photo.png", publicationPNG(t, 13))
	lease, generation := f.preparePublication(t, "full", ".")
	if _, err := f.inventory.PublishPart(t.Context(), lease, generation.ID); err != nil {
		t.Fatal(err)
	}
	job, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "running" || job.TotalKnown || job.Done != 3 {
		t.Fatal("one source claimed all-source completion", job, err)
	}
	second, err := f.inventory.Build(t.Context(), lease, other.ID, other.ConfigVersion, "full", ".")
	if err != nil {
		t.Fatal(err)
	}
	second, err = f.inventory.ProbeMetadata(t.Context(), lease, second.ID, media.DefaultInspectPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.Publish(t.Context(), lease, second.ID); err != nil {
		t.Fatal(err)
	}
	job, err = f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || job.Status != "succeeded" || !job.TotalKnown || job.Done != 6 || job.Succeeded != 6 {
		t.Fatal("multi-source cumulative progress/completion", job, err)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM inventory_generations WHERE job_id=? AND state='published'`, lease.Job.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal("source parts missing", count, err)
	}
}

func TestPublicationPersonIdentityUsesSourceAndFullPathNotEqualLabels(t *testing.T) {
	f := inventorySetup(t)
	mapping := library.Mapping{AlbumDepth: 3, PersonDepth: 2}
	var err error
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{Mapping: &mapping})
	if err != nil {
		t.Fatal(err)
	}
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "North/Alice/Trip/photo.png", publicationPNG(t, 12))
	writeInput(t, root, "South/Alice/Trip/photo.png", publicationPNG(t, 13))
	f.publish(t, "full", ".")
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM people WHERE source_name='Alice' AND source_id=?`, f.source.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal("equal labels collapsed distinct people", count, err)
	}
	var paths int
	if err := f.db.QueryRow(`SELECT COUNT(DISTINCT relative_path) FROM people`).Scan(&paths); err != nil || paths != 2 {
		t.Fatal(paths, err)
	}
}
