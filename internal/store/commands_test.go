package store

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"gallerygo/migrations"
)

func TestCommandReceiptsAreActorScopedAtomicAndExpiring(t *testing.T) {
	db := testDB(t)
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at,scope_id) VALUES('actor','actor','not-a-login-credential','admin',0,0,'first-scope'),('other','other','not-a-login-credential','admin',0,0,'second-scope'); CREATE TABLE example(id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	command := Command{Actor: "actor", Key: NewID(), Operation: "group.create", Metadata: `{"name":"Family"}`}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var replay map[string]string
	if found, err := command.Replay(t.Context(), tx, now, &replay); found || err != nil {
		t.Fatalf("unexpected receipt: %v %v", found, err)
	}
	if _, err := tx.Exec(`INSERT INTO example VALUES('business-row')`); err != nil {
		t.Fatal(err)
	}
	result := map[string]string{"id": "business-row"}
	if err := command.Save(t.Context(), tx, result, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if found, err := command.Replay(t.Context(), tx, now, &replay); !found || err != nil || replay["id"] != result["id"] {
		t.Fatalf("replay=%v result=%v error=%v", found, replay, err)
	}
	other := command
	other.Actor = "other"
	if found, err := other.Replay(t.Context(), tx, now, &replay); found || err != nil {
		t.Fatal("receipt crossed actor boundary", found, err)
	}
	other = command
	other.Operation = "library.create"
	if _, err := other.Replay(t.Context(), tx, now, &replay); !errors.Is(err, ErrIdempotencyReuse) {
		t.Fatal("key crossed operation boundary", err)
	}
	other = command
	other.Metadata = `{"name":"Different"}`
	if _, err := other.Replay(t.Context(), tx, now, &replay); !errors.Is(err, ErrIdempotencyReuse) {
		t.Fatal("key crossed request boundary", err)
	}
	if found, err := command.Replay(t.Context(), tx, now.Add(24*time.Hour), &replay); found || err != nil {
		t.Fatal("expired receipt replayed", found, err)
	}
	if err := command.Save(t.Context(), tx, result, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var expiry int64
	if err := db.QueryRow(`SELECT expires_at FROM mutation_receipts`).Scan(&expiry); err != nil || expiry != now.Add(24*time.Hour).UnixMilli() {
		t.Fatal("receipt rewrite did not roll back", expiry, err)
	}
}

func TestCommandReceiptMigrationPreservesPendingResults(t *testing.T) {
	db := testDB(t)
	old := fstest.MapFS{}
	names, err := fs.Glob(migrations.Files, "000[123]_*.sql")
	if err != nil || len(names) != 3 {
		t.Fatalf("old schema fixtures: %v %v", names, err)
	}
	for _, name := range names {
		data, err := migrations.Files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		old[name] = &fstest.MapFile{Data: data}
	}
	if err := Migrate(t.Context(), db, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id,username,password_hash,role,created_at,updated_at,scope_id) VALUES('actor','actor','hash-fixture','admin',0,0,'scope'); INSERT INTO admin_user_requests(actor_user_id,request_key,operation,request_json,password_hash,result_json,expires_at) VALUES('actor','old-pending-result','create','{}','hash-fixture','{"id":"existing"}',9999999999999)`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	command := Command{Actor: "actor", Key: "old-pending-result", Operation: "create", Metadata: "{}", PasswordHash: "hash-fixture"}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var result map[string]string
	if found, err := command.Replay(t.Context(), tx, time.Now(), &result); !found || err != nil || result["id"] != "existing" {
		t.Fatalf("migrated receipt lost: %v %v %v", found, result, err)
	}
	command.Key = "invalid"
	if _, err := command.Replay(t.Context(), tx, time.Now(), &result); !errors.Is(err, ErrIdempotencyKey) {
		t.Fatal("invalid replay key accepted", err)
	}
	if err := command.Save(t.Context(), tx, result, time.Now()); !errors.Is(err, ErrIdempotencyKey) {
		t.Fatal("invalid save key accepted", err)
	}
}
