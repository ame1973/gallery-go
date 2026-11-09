package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gallerygo/internal/access"
	"gallerygo/internal/auth"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

const collectionTestPassword = "a collection integration test passphrase"

type collectionFixture struct {
	db           *sql.DB
	service      *access.Service
	auth         *auth.Service
	owner, other auth.Session
}

func newCollectionFixture(t *testing.T) collectionFixture {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "collection.db"))
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
	if _, err := a.Bootstrap(t.Context(), "owner", collectionTestPassword); err != nil {
		t.Fatal(err)
	}
	owner, err := a.Login(t.Context(), "owner", collectionTestPassword, "192.0.2.1", "collection test", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateUser(t.Context(), owner, auth.NewUser{Username: "other", Password: collectionTestPassword, Role: "user", Active: true}, store.NewID()); err != nil {
		t.Fatal(err)
	}
	other, err := a.Login(t.Context(), "other", collectionTestPassword, "192.0.2.2", "collection test", "")
	if err != nil {
		t.Fatal(err)
	}
	return collectionFixture{db: db, service: access.New(db, a), auth: a, owner: owner, other: other}
}

func collectionStats(t *testing.T, f collectionFixture) [5]int64 {
	t.Helper()
	var result [5]int64
	if err := f.db.QueryRow(`SELECT
 (SELECT COUNT(*) FROM favorite_collections WHERE user_id=?),
 (SELECT COUNT(*) FROM change_log WHERE actor_scope=? AND kind='favorite_collection'),
 (SELECT COUNT(*) FROM outbox_events o JOIN change_log c ON c.seq=o.change_seq WHERE c.actor_scope=? AND c.kind='favorite_collection'),
 (SELECT COUNT(*) FROM audit_events WHERE actor_user_id=? AND event_type IN ('favorite_collection_created','favorite_collection_renamed')),
 (SELECT COUNT(*) FROM mutation_receipts WHERE actor_user_id=? AND operation IN ('favorite_collection.create','favorite_collection.rename'))`, f.owner.User.ID, f.owner.User.ScopeID, f.owner.User.ScopeID, f.owner.User.ID, f.owner.User.ID).Scan(&result[0], &result[1], &result[2], &result[3], &result[4]); err != nil {
		t.Fatal(err)
	}
	return result
}

func requireCollectionError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("collection error=%v want=%v", err, want)
	}
}

func TestFavoriteCollectionsServiceOwnershipReceiptsEventsAndRealFavorites(t *testing.T) {
	f := newCollectionFixture(t)
	ctx := t.Context()
	initial, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{}, "", 30)
	if err != nil || initial == nil || len(initial) != 0 {
		t.Fatal("initial collection list is not an empty owned page", initial, err)
	}
	rootKey := store.NewID()
	root, err := f.service.CreateFavoriteCollection(ctx, f.owner, "  Root  ", nil, rootKey)
	if err != nil || root.ID == "" || root.ParentID != nil || root.Name != "Root" || root.Version != 1 || !root.CreatedAt.Equal(root.UpdatedAt) {
		t.Fatal("root creation lost personal metadata", root, err)
	}
	if replay, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Root", nil, rootKey); err != nil || !reflect.DeepEqual(replay, root) {
		t.Fatal("canonical create retry did not return its historical result", replay, root, err)
	}
	_, err = f.service.CreateFavoriteCollection(ctx, f.owner, "Different", nil, rootKey)
	requireCollectionError(t, err, store.ErrIdempotencyReuse)
	_, err = f.service.CreateFavoriteCollection(ctx, f.owner, "Root", nil, store.NewID())
	requireCollectionError(t, err, access.ErrFavoriteCollectionName)
	child, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Child", &root.ID, store.NewID())
	if err != nil || child.ParentID == nil || *child.ParentID != root.ID {
		t.Fatal("owned parent was not retained", child, err)
	}
	otherRoot, err := f.service.CreateFavoriteCollection(ctx, f.other, "Root", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.CreateFavoriteCollection(ctx, f.owner, "Foreign child", &otherRoot.ID, store.NewID())
	requireCollectionError(t, err, sql.ErrNoRows)
	_, err = f.service.CreateFavoriteCollection(ctx, f.owner, "Missing child", new("missing"), store.NewID())
	requireCollectionError(t, err, sql.ErrNoRows)
	_, err = f.service.FavoriteCollection(ctx, f.owner, otherRoot.ID)
	requireCollectionError(t, err, sql.ErrNoRows)
	forged := f.other
	forged.User = f.owner.User
	_, err = f.service.FavoriteCollection(ctx, forged, root.ID)
	requireCollectionError(t, err, sql.ErrNoRows)
	_, err = f.service.RenameFavoriteCollection(ctx, forged, root.ID, 1, "Forged", store.NewID())
	requireCollectionError(t, err, sql.ErrNoRows)
	forgedCreate, err := f.service.CreateFavoriteCollection(ctx, forged, "Actual actor", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.FavoriteCollection(ctx, f.other, forgedCreate.ID); err != nil {
		t.Fatal("forged user DTO did not remain bound to the real session", err)
	}
	_, err = f.service.FavoriteCollection(ctx, f.owner, forgedCreate.ID)
	requireCollectionError(t, err, sql.ErrNoRows)
	_, err = f.service.FavoriteCollections(ctx, f.other, access.FavoriteCollectionFilter{ParentID: &root.ID}, "", 30)
	requireCollectionError(t, err, sql.ErrNoRows)

	second, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Second", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Child", &second.ID, store.NewID()); err != nil {
		t.Fatal("same name under another parent was rejected", err)
	}
	for _, name := range []string{"child", "é", "é"} {
		if _, err := f.service.CreateFavoriteCollection(ctx, f.owner, name, &root.ID, store.NewID()); err != nil {
			t.Fatal("literal binary name variants were incorrectly merged", name, err)
		}
	}
	roots, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{RootOnly: true}, "", 101)
	if err != nil || len(roots) != 2 {
		t.Fatal("root-only list included children or another owner", roots, err)
	}
	children, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{ParentID: &root.ID}, "", 101)
	if err != nil || len(children) != 4 {
		t.Fatal("direct child list lost owned literal names", children, err)
	}
	all, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{}, "", 101)
	if err != nil || len(all) != 7 {
		t.Fatal("flat owned list lost folders or included another account", all, err)
	}
	var expected []string
	for _, item := range all {
		expected = append(expected, item.ID)
	}
	if !slices.IsSorted(expected) {
		t.Fatal("flat collection list lost stable ID order", expected)
	}
	var paged []string
	for after := ""; ; {
		items, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{}, after, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		paged = append(paged, items[0].ID)
		after = items[0].ID
	}
	if !slices.Equal(paged, expected) {
		t.Fatal("collection keysets repeated or lost folders", paged, expected)
	}

	if _, err := f.db.Exec(`INSERT INTO libraries(id,name,created_at) VALUES('collection-library','Library',0);
 INSERT INTO albums(id,library_id,source_name,created_at,updated_at) VALUES('collection-album','collection-library','Album',0,0)`); err != nil {
		t.Fatal(err)
	}
	favorite, err := f.service.SetAlbumFavorite(ctx, f.owner, "collection-album", 1, true, &child.ID, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	album, err := f.service.Album(ctx, f.owner, "collection-album")
	if err != nil {
		t.Fatal(err)
	}
	renameKey := store.NewID()
	renamed, err := f.service.RenameFavoriteCollection(ctx, f.owner, root.ID, 1, "Renamed", renameKey)
	if err != nil || renamed.Name != "Renamed" || renamed.Version != 2 || renamed.UpdatedAt.Before(root.UpdatedAt) {
		t.Fatal("rename did not publish a monotonic personal version", renamed, err)
	}
	beforeNoop := collectionStats(t, f)
	noop, err := f.service.RenameFavoriteCollection(ctx, f.owner, root.ID, 2, " Renamed ", store.NewID())
	afterNoop := collectionStats(t, f)
	afterNoop[4]--
	if err != nil || !reflect.DeepEqual(noop, renamed) || afterNoop != beforeNoop {
		t.Fatal("same-name no-op changed row/events rather than only a receipt", noop, beforeNoop, afterNoop, err)
	}
	_, err = f.service.RenameFavoriteCollection(ctx, f.owner, root.ID, 2, "Second", store.NewID())
	requireCollectionError(t, err, access.ErrFavoriteCollectionName)
	secondRename, err := f.service.RenameFavoriteCollection(ctx, f.owner, root.ID, 2, "Latest", store.NewID())
	if err != nil || secondRename.Version != 3 {
		t.Fatal("second rename failed", secondRename, err)
	}
	replay, err := f.service.RenameFavoriteCollection(ctx, f.owner, root.ID, 1, "Renamed", renameKey)
	if err != nil || !reflect.DeepEqual(replay, renamed) {
		t.Fatal("rename retry lost historical result", replay, renamed, err)
	}
	current, err := f.service.FavoriteCollection(ctx, f.owner, root.ID)
	if err != nil || !reflect.DeepEqual(current, secondRename) {
		t.Fatal("rename replay restored an obsolete name", current, secondRename, err)
	}
	_, err = f.service.RenameFavoriteCollection(ctx, f.owner, root.ID, 1, "Renamed", store.NewID())
	requireCollectionError(t, err, auth.ErrConflict)
	if _, err := f.db.Exec(`UPDATE mutation_receipts SET expires_at=0 WHERE actor_user_id=? AND request_key=?`, f.owner.User.ID, renameKey); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.RenameFavoriteCollection(ctx, f.owner, root.ID, 1, "Renamed", renameKey)
	requireCollectionError(t, err, auth.ErrConflict)
	_, err = f.service.RenameFavoriteCollection(ctx, f.other, root.ID, 3, "Forbidden", store.NewID())
	requireCollectionError(t, err, sql.ErrNoRows)
	if got, err := f.service.AlbumFavorite(ctx, f.owner, "collection-album"); err != nil || !reflect.DeepEqual(got, favorite) {
		t.Fatal("folder rename changed an existing favorite", got, favorite, err)
	}
	if got, err := f.service.Album(ctx, f.owner, "collection-album"); err != nil || !reflect.DeepEqual(got, album) {
		t.Fatal("folder rename changed shared album metadata", got, album, err)
	}
	fresh, err := f.auth.Authenticate(ctx, f.owner.Token)
	if err != nil || fresh.User.ScopeID != f.owner.User.ScopeID {
		t.Fatal("private folder operations rotated an authorization scope", fresh, err)
	}
	var scope, operation, payload string
	if err := f.db.QueryRow(`SELECT actor_scope,operation,payload_json FROM change_log WHERE kind='favorite_collection' AND resource_id=? ORDER BY seq DESC LIMIT 1`, root.ID).Scan(&scope, &operation, &payload); err != nil {
		t.Fatal(err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &event); err != nil || len(event) != 6 || scope != f.owner.User.ScopeID || operation != "upsert" {
		t.Fatal("collection journal is not complete and actor-private", scope, operation, payload, err)
	}
	for _, field := range []string{"id", "parent_id", "name", "created_at", "updated_at", "version"} {
		if _, ok := event[field]; !ok {
			t.Fatal("collection journal omitted a field", field, payload)
		}
	}

	parentKey, childKey := store.NewID(), store.NewID()
	pastParent, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Past parent", nil, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	pastChild, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Past child", &pastParent.ID, childKey)
	if err != nil {
		t.Fatal(err)
	}
	pastRenameKey := store.NewID()
	if _, err := f.service.RenameFavoriteCollection(ctx, f.owner, pastChild.ID, 1, "Past renamed", pastRenameKey); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`DELETE FROM favorite_collections WHERE id=?`, pastChild.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`DELETE FROM favorite_collections WHERE id=?`, pastParent.ID); err != nil {
		t.Fatal(err)
	}
	past, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Past child", &pastParent.ID, childKey)
	if err != nil || !reflect.DeepEqual(past, pastChild) {
		t.Fatal("create receipt lost its historical own result after parent deletion", past, pastChild, err)
	}
	_, err = f.service.FavoriteCollection(ctx, f.owner, pastChild.ID)
	requireCollectionError(t, err, sql.ErrNoRows)
	_, err = f.service.RenameFavoriteCollection(ctx, f.owner, pastChild.ID, 1, "Past renamed", pastRenameKey)
	requireCollectionError(t, err, sql.ErrNoRows)

	for _, value := range []string{"", " padded", "a/b", `a\b`, "bad\x00", "bad" + string(rune(0x200d)), strings.Repeat("x", 65), string([]byte{0xff})} {
		_, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Bad parent", &value, store.NewID())
		requireCollectionError(t, err, access.ErrFavoriteCollectionParent)
	}
	for _, name := range []string{"", "   ", "bad\x00", strings.Repeat("x", 101), string([]byte{0xff})} {
		_, err := f.service.CreateFavoriteCollection(ctx, f.owner, name, nil, store.NewID())
		requireCollectionError(t, err, access.ErrName)
	}
	if _, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{ParentID: &root.ID, RootOnly: true}, "", 30); !errors.Is(err, access.ErrFavoriteCollectionParent) {
		t.Fatal("ambiguous root/parent filter was accepted", err)
	}
	for _, limit := range []int{0, 102} {
		if _, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{}, "", limit); !errors.Is(err, access.ErrGroupSet) {
			t.Fatal("unbounded collection page limit was accepted", limit, err)
		}
	}
	if _, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{}, strings.Repeat("x", 65), 30); !errors.Is(err, access.ErrFavoriteCollectionParent) {
		t.Fatal("unbounded collection page position was accepted", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.service.FavoriteCollections(cancelled, f.owner, access.FavoriteCollectionFilter{}, "", 30); !errors.Is(err, context.Canceled) {
		t.Fatal("collection list ignored cancellation", err)
	}
	if err := f.auth.Revoke(ctx, f.other, f.other.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.FavoriteCollections(ctx, f.other, access.FavoriteCollectionFilter{}, "", 30)
	requireCollectionError(t, err, auth.ErrUnauthenticated)
}

func TestFavoriteCollectionsService100KUsesScopedSparseParentKeysets(t *testing.T) {
	f := newCollectionFixture(t)
	ctx := t.Context()
	sparse, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Sparse", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	dense, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Dense", nil, store.NewID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.CreateFavoriteCollection(ctx, f.other, "Other owner", nil, store.NewID()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<99999)
 INSERT INTO favorite_collections(id,user_id,parent_id,name,created_at,updated_at,version)
 SELECT printf('scale%06d',i),?,CASE WHEN i%997=0 THEN ? ELSE ? END,printf('Name %06d',i),0,0,1 FROM n`, f.owner.User.ID, sparse.ID, dense.ID); err != nil {
		t.Fatal(err)
	}
	const query = `SELECT id,parent_id,name,created_at,updated_at,version FROM favorite_collections WHERE user_id=? AND parent_id=? AND id>? ORDER BY id LIMIT ?`
	rows, err := f.db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+query, f.owner.User.ID, sparse.ID, "scale050000", 11)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		fmt.Fprintln(&plan, detail)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || !strings.Contains(plan.String(), "SEARCH") || !strings.Contains(plan.String(), "idx_collection_owner_parent_page") || strings.Contains(plan.String(), "TEMP B-TREE") || strings.Contains(query, "OFFSET") {
		t.Fatal("sparse owned collection page lost its indexed ID keyset", plan.String(), err)
	}
	page, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{ParentID: &sparse.ID}, "scale050000", 11)
	if err != nil || len(page) != 11 {
		t.Fatal("sparse collection page was not bounded", len(page), err)
	}
	previous := "scale050000"
	for _, value := range page {
		if value.ID <= previous || value.ParentID == nil || *value.ParentID != sparse.ID {
			t.Fatal("sparse collection page lost order or crossed its parent", value, previous)
		}
		previous = value.ID
	}
	roots, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{RootOnly: true}, "", 101)
	if err != nil || len(roots) != 2 {
		t.Fatal("large root page included child/foreign folders", roots, err)
	}
	other, err := f.service.FavoriteCollections(ctx, f.other, access.FavoriteCollectionFilter{}, "", 101)
	if err != nil || len(other) != 1 {
		t.Fatal("large catalog leaked another owner's folders", other, err)
	}
}

func TestFavoriteCollectionsServiceConcurrentReceiptsFailuresAndBounds(t *testing.T) {
	f := newCollectionFixture(t)
	ctx := t.Context()
	key := store.NewID()
	start := make(chan struct{})
	results := make(chan struct {
		value access.FavoriteCollection
		err   error
	}, 8)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			<-start
			value, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Concurrent", nil, key)
			results <- struct {
				value access.FavoriteCollection
				err   error
			}{value, err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	var created access.FavoriteCollection
	for result := range results {
		if result.err != nil || created.ID != "" && !reflect.DeepEqual(created, result.value) {
			t.Fatal("same-key concurrent create duplicated or retargeted a folder", result, created)
		}
		created = result.value
	}
	if stats := collectionStats(t, f); stats != [5]int64{1, 1, 1, 1, 1} {
		t.Fatal("same-key create duplicated row/event/outbox/audit/receipt", stats)
	}
	start = make(chan struct{})
	renames := make(chan error, 8)
	for i := range 8 {
		workers.Go(func() {
			<-start
			_, err := f.service.RenameFavoriteCollection(ctx, f.owner, created.ID, 1, fmt.Sprintf("Name %d", i), store.NewID())
			renames <- err
		})
	}
	close(start)
	workers.Wait()
	close(renames)
	changed, conflicts := 0, 0
	for err := range renames {
		switch {
		case err == nil:
			changed++
		case errors.Is(err, auth.ErrConflict):
			conflicts++
		default:
			t.Fatal("concurrent rename escaped the version contract", err)
		}
	}
	if changed != 1 || conflicts != 7 {
		t.Fatal("concurrent stale renames were not serialized", changed, conflicts)
	}
	for _, injection := range []struct{ name, table, predicate string }{
		{"change", "change_log", `NEW.kind='favorite_collection'`},
		{"outbox", "outbox_events", `EXISTS(SELECT 1 FROM change_log WHERE seq=NEW.change_seq AND kind='favorite_collection')`},
		{"audit", "audit_events", `NEW.event_type='favorite_collection_created'`},
		{"receipt", "mutation_receipts", `NEW.operation='favorite_collection.create'`},
	} {
		query := `CREATE TRIGGER fail_collection_` + injection.name + ` BEFORE INSERT ON ` + injection.table + ` WHEN ` + injection.predicate + ` BEGIN SELECT RAISE(ABORT,'injected private failure'); END`
		if _, err := f.db.Exec(query); err != nil {
			t.Fatal(err)
		}
		before := collectionStats(t, f)
		if _, err := f.service.CreateFavoriteCollection(ctx, f.owner, "Rollback "+injection.name, nil, store.NewID()); err == nil {
			t.Fatal("injected late failure was ignored", injection.name)
		}
		if after := collectionStats(t, f); after != before {
			t.Fatal("failed create left partial row/change/outbox/audit/receipt", injection.name, before, after)
		}
		if _, err := f.db.Exec(`DROP TRIGGER fail_collection_` + injection.name); err != nil {
			t.Fatal(err)
		}
	}
	current, err := f.service.FavoriteCollection(ctx, f.owner, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`CREATE TRIGGER fail_collection_rename BEFORE INSERT ON mutation_receipts WHEN NEW.operation='favorite_collection.rename' BEGIN SELECT RAISE(ABORT,'injected private failure'); END`); err != nil {
		t.Fatal(err)
	}
	before := collectionStats(t, f)
	if _, err := f.service.RenameFavoriteCollection(ctx, f.owner, created.ID, current.Version, "Failed rename", store.NewID()); err == nil {
		t.Fatal("late rename receipt failure was ignored")
	}
	after, err := f.service.FavoriteCollection(ctx, f.owner, created.ID)
	if err != nil || !reflect.DeepEqual(current, after) || collectionStats(t, f) != before {
		t.Fatal("failed rename changed row/events/receipt", current, after, err)
	}
	if _, err := f.db.Exec(`DROP TRIGGER fail_collection_rename`); err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)
	if _, err := f.db.Exec(`UPDATE favorite_collections SET updated_at=? WHERE id=?`, future.UnixMilli(), created.ID); err != nil {
		t.Fatal(err)
	}
	current, err = f.service.RenameFavoriteCollection(ctx, f.owner, created.ID, current.Version, "Clock regression", store.NewID())
	if err != nil || !current.UpdatedAt.Equal(future) {
		t.Fatal("collection update time moved backwards", current, future, err)
	}
	if _, err := f.db.Exec(`UPDATE favorite_collections SET updated_at=? WHERE id=?`, int64(math.MaxInt64), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.FavoriteCollection(ctx, f.owner, created.ID); err == nil {
		t.Fatal("invalid persisted time was exposed as a collection")
	}
	if _, err := f.db.Exec(`UPDATE favorite_collections SET updated_at=?,version=? WHERE id=?`, future.UnixMilli(), int64(math.MaxInt64), created.ID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{" padded ", "bad\nname", strings.Repeat("x", 101), string([]byte{0xff})} {
		if _, err := f.db.Exec(`UPDATE favorite_collections SET name=? WHERE id=?`, name, created.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.FavoriteCollection(ctx, f.owner, created.ID); err == nil {
			t.Fatal("invalid stored name was silently normalized or exposed", name)
		}
		if _, err := f.service.FavoriteCollections(ctx, f.owner, access.FavoriteCollectionFilter{}, "", 30); err == nil {
			t.Fatal("invalid stored name was emitted through a collection page", name)
		}
	}
	if _, err := f.db.Exec(`UPDATE favorite_collections SET name='Clock regression' WHERE id=?`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RenameFavoriteCollection(ctx, f.owner, created.ID, math.MaxInt64, "Clock regression", store.NewID()); err != nil {
		t.Fatal("true no-op was rejected at maximum version", err)
	}
	_, err = f.service.RenameFavoriteCollection(ctx, f.owner, created.ID, math.MaxInt64, "Overflow", store.NewID())
	requireCollectionError(t, err, auth.ErrConflict)
	if _, err := f.db.Exec(`UPDATE users SET active=0 WHERE id=?`, f.owner.User.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.FavoriteCollection(ctx, f.owner, created.ID)
	requireCollectionError(t, err, auth.ErrUnauthenticated)
	_, err = f.service.CreateFavoriteCollection(ctx, f.owner, "Concurrent", nil, key)
	requireCollectionError(t, err, auth.ErrUnauthenticated)
}
