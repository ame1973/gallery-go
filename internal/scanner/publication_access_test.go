package scanner

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/jobs"
	"gallerygo/internal/library"
	"gallerygo/internal/media"
	"gallerygo/internal/store"
)

func TestPublicationFileMovePreservesCoverPreferenceAndRevokesOldScopes(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	a := writeInput(t, root, "A/selected.png", publicationPNG(t, 12))
	writeInput(t, root, "B/other.png", publicationPNG(t, 13))
	f.publish(t, "full", ".")
	var id, album string
	if err := f.db.QueryRow(`SELECT id,album_id FROM media WHERE file_name='selected.png'`).Scan(&id, &album); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE albums SET cover_override_id=? WHERE id=?`, id, album); err != nil {
		t.Fatal(err)
	}
	var previousScope string
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.owner.User.ID).Scan(&previousScope); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root.Path, "B", "selected.png")
	if err := os.Rename(a, moved); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	var projected sql.NullString
	var preference, currentScope string
	if err := f.db.QueryRow(`SELECT cover_media_id,cover_override_id FROM albums WHERE id=?`, album).Scan(&projected, &preference); err != nil || projected.Valid || preference != id {
		t.Fatal("cross-album cover leaked or preference was destroyed", projected, preference, err)
	}
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.owner.User.ID).Scan(&currentScope); err != nil || currentScope == previousScope {
		t.Fatal("old authorization projection survived an ownership move", currentScope, err)
	}
	var revokes int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND operation='revoke' AND kind='scope'`, previousScope).Scan(&revokes); err != nil || revokes != 1 {
		t.Fatal("old-scope revoke missing", revokes, err)
	}
	if err := os.Rename(moved, a); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	if err := f.db.QueryRow(`SELECT cover_media_id,cover_override_id FROM albums WHERE id=?`, album).Scan(&projected, &preference); err != nil || !projected.Valid || projected.String != id || preference != id {
		t.Fatal("returning media did not restore retained manual cover", projected, preference, err)
	}
	var validSessions int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=? AND revoked_at IS NULL`, f.owner.User.ID).Scan(&validSessions); err != nil || validSessions != 1 {
		t.Fatal("ownership scope rotation unnecessarily revoked login", validSessions, err)
	}
}

func TestPublicationMixedFolderMergeKeepsOldAlbumsInsteadOfGuessing(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	a := writeInput(t, root, "A/a.png", publicationPNG(t, 12))
	b := writeInput(t, root, "B/b.png", publicationPNG(t, 13))
	f.publish(t, "full", ".")
	if err := os.Mkdir(filepath.Join(root.Path, "C"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(a, filepath.Join(root.Path, "C", "a.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(b, filepath.Join(root.Path, "C", "b.png")); err != nil {
		t.Fatal(err)
	}
	generation := f.publish(t, "full", ".")
	var albums, medias, issues int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM albums),(SELECT COUNT(*) FROM media),(SELECT COUNT(*) FROM inventory_issues WHERE generation_id=? AND error_code='AMBIGUOUS_ALBUM_RENAME')`, generation.ID).Scan(&albums, &medias, &issues); err != nil || albums != 3 || medias != 2 || issues != 1 {
		t.Fatal("mixed folder transition guessed an album identity", albums, medias, issues, err)
	}
}

func TestPublicationRehomesOnlyAfterHealthAndRevokesBothLibraryProjections(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "Album/photo.png", publicationPNG(t, 12))
	f.publish(t, "full", ".")
	retainedAlbum := store.NewID()
	if _, err := f.db.Exec(`INSERT INTO albums(id,library_id,source_id,source_name,display_name,relative_path,created_at,updated_at) VALUES(?,?,?,'Retained','Retained override','Retained',0,0)`, retainedAlbum, f.source.LibraryID, f.source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO favorites(user_id,album_id,created_at) VALUES(?,?,0)`, f.owner.User.ID, retainedAlbum); err != nil {
		t.Fatal(err)
	}
	policies := access.New(f.db, f.auth)
	destination, err := policies.CreateLibrary(t.Context(), f.owner, "Destination", true, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	readers := make([]auth.Session, 0, 2)
	for _, setting := range []struct{ name, library string }{{"old-reader", f.source.LibraryID}, {"new-reader", destination.ID}} {
		user, group := store.NewID(), store.NewID()
		if _, err := f.db.Exec(`INSERT INTO users(id,username,password_hash,role,active,scope_id,created_at,updated_at) SELECT ?,?,password_hash,'user',1,?,0,0 FROM users WHERE id=?`, user, setting.name, store.NewID(), f.owner.User.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`INSERT INTO permission_groups(id,name,created_at) VALUES(?,?,0)`, group, setting.name); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`INSERT INTO user_groups VALUES(?,?)`, user, group); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`INSERT INTO library_access VALUES(?,?)`, setting.library, group); err != nil {
			t.Fatal(err)
		}
		session, err := f.auth.Login(t.Context(), setting.name, "a private inventory integration test phrase", "192.0.2.2", "publication test", "")
		if err != nil {
			t.Fatal(err)
		}
		readers = append(readers, session)
	}
	var id, album string
	if err := f.db.QueryRow(`SELECT id,album_id FROM media`).Scan(&id, &album); err != nil {
		t.Fatal(err)
	}
	f.source, err = f.sources.Get(t.Context(), f.owner, f.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.source, err = f.sources.Update(t.Context(), f.owner, f.source.ID, f.source.Version, library.SourcePatch{LibraryID: &destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	if _, err := service.Get(t.Context(), f.owner, id); err == nil {
		t.Fatal("configuration edit exposed mismatched old index")
	}
	previous := make([]string, len(readers))
	for i, reader := range readers {
		if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, reader.User.ID).Scan(&previous[i]); err != nil {
			t.Fatal(err)
		}
	}
	f.publish(t, "full", ".")
	_, err = service.Get(t.Context(), readers[0], id)
	walkError(t, err, sql.ErrNoRows)
	if _, err := service.Get(t.Context(), readers[1], id); err != nil {
		t.Fatal("healthy publication failed to rehome authorized media", err)
	}
	for i, reader := range readers {
		var current string
		var revoked int
		if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, reader.User.ID).Scan(&current); err != nil || current == previous[i] {
			t.Fatal("library projection not rotated", i, current, err)
		}
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND operation='revoke'`, previous[i]).Scan(&revoked); err != nil || revoked != 1 {
			t.Fatal("old projection revoke missing", i, revoked, err)
		}
	}
	var retained int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM albums a JOIN favorites f ON f.album_id=a.id WHERE a.id=? AND a.library_id=? AND a.display_name='Retained override'`, retainedAlbum, destination.ID).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("healthy rehome lost or permanently hid a retained empty album", retained, err)
	}
	var currentAlbum string
	if err := f.db.QueryRow(`SELECT album_id FROM media WHERE id=?`, id).Scan(&currentAlbum); err != nil || currentAlbum != album {
		t.Fatal("library rehome replaced logical album", currentAlbum, err)
	}
}

func TestPublicationLateFailureRollsBackOwnershipAndScopeRevokes(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	name := writeInput(t, root, "A/photo.png", publicationPNG(t, 12))
	writeInput(t, root, "B/other.png", publicationPNG(t, 13))
	first := f.publish(t, "full", ".")
	var mediaID, albumID, scope string
	var floor int64
	if err := f.db.QueryRow(`SELECT id,album_id FROM media WHERE file_name='photo.png'`).Scan(&mediaID, &albumID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.owner.User.ID).Scan(&scope); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(name, filepath.Join(root.Path, "B", "photo.png")); err != nil {
		t.Fatal(err)
	}
	lease, generation := f.preparePublication(t, "full", ".")
	if err := f.db.QueryRow(`SELECT MAX(seq) FROM change_log`).Scan(&floor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_ownership_publication BEFORE INSERT ON audit_events WHEN NEW.event_type='inventory_published' BEGIN SELECT RAISE(ABORT,'injected late ownership failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.Publish(t.Context(), lease, generation.ID); err == nil {
		t.Fatal("ownership transaction survived failed audit")
	}
	var currentAlbum, currentScope string
	var number int64
	var revokes int
	if err := f.db.QueryRow(`SELECT album_id FROM media WHERE id=?`, mediaID).Scan(&currentAlbum); err != nil || currentAlbum != albumID {
		t.Fatal("media moved without its transaction", currentAlbum, err)
	}
	if err := f.db.QueryRow(`SELECT scope_id FROM users WHERE id=?`, f.owner.User.ID).Scan(&currentScope); err != nil || currentScope != scope {
		t.Fatal("scope rotation escaped rollback", currentScope, err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE seq>? AND kind='scope' AND operation='revoke'`, floor).Scan(&revokes); err != nil || revokes != 0 {
		t.Fatal("old-scope revoke escaped rollback", revokes, err)
	}
	if err := f.db.QueryRow(`SELECT scan_generation FROM sources`).Scan(&number); err != nil || number != first.Number {
		t.Fatal("source generation escaped rollback", number, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_ownership_publication`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.Publish(t.Context(), lease, generation.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE seq>? AND actor_scope=? AND kind='scope' AND operation='revoke'`, floor, scope).Scan(&revokes); err != nil || revokes != 1 {
		t.Fatal("successful ownership retry lost the original-scope revoke", revokes, err)
	}
}

func TestPublicationConcurrentAndCancelledOwnersCannotDuplicateOrPublish(t *testing.T) {
	f := inventorySetup(t)
	writeInput(t, media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}, "photo.png", publicationPNG(t, 12))
	lease, generation := f.preparePublication(t, "full", ".")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := f.inventory.Publish(t.Context(), lease, generation.ID); results <- err })
	}
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else {
			walkError(t, err, jobs.ErrLeaseLost)
		}
	}
	if succeeded != 1 {
		t.Fatal("publication ownership was not exclusive", succeeded)
	}
	var count int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM media`).Scan(&count); err != nil || count != 1 {
		t.Fatal("concurrent publication duplicated media", count, err)
	}
	lease, generation = f.preparePublication(t, "incremental", ".")
	if _, err := f.db.Exec(`UPDATE jobs SET cancel_requested=1 WHERE id=?`, lease.Job.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.inventory.Publish(t.Context(), lease, generation.ID)
	walkError(t, err, jobs.ErrCancelled)
	var number int64
	if err := f.db.QueryRow(`SELECT scan_generation FROM sources`).Scan(&number); err != nil || number != 1 {
		t.Fatal("cancelled generation advanced source", number, err)
	}
}
