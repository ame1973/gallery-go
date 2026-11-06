//go:build linux && (amd64 || arm64)

package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/jobs"
	"gallerygo/internal/store"
	"gallerygo/migrations"
)

type stillInputFixture struct {
	svc        *Service
	queue      *jobs.Queue
	lease      jobs.Lease
	db         *sql.DB
	root, file string
	data       []byte
}

func newStillInputFixture(t *testing.T) stillInputFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".marker"), []byte("owned mount"), 0o400); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "owned.png")
	if err := os.WriteFile(file, encoded.Bytes(), 0o400); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := Inspect(t.Context(), input, "owned.png", DefaultInspectPolicy())
	input.Close()
	if err != nil || inspection.Status != "ok" {
		t.Fatal(inspection, err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded.Bytes())
	hash := hex.EncodeToString(digest[:])
	db, err := store.Open(t.Context(), filepath.Join(base, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := store.Migrate(t.Context(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	configured := SourceRoot{Path: root, Marker: ".marker"}
	data, _ := json.Marshal(inspection)
	queries := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO libraries(id,name,created_at) VALUES('input-library','Input',0)`, nil},
		{`INSERT INTO sources(id,library_id,name,root_path,root_config_sha256,mapping_json,state,created_at) VALUES('input-source','input-library','owned',?,?,'{}','healthy',0)`, []any{root, configured.Signature()}},
		{`INSERT INTO albums(id,library_id,source_id,source_name,created_at,updated_at) VALUES('input-album','input-library','input-source','Input',0,0)`, nil},
		{`INSERT INTO source_entries(id,source_id,relative_path,size_bytes,mtime_ns,sha256,file_identity,source_config_version,updated_at) VALUES('input-entry','input-source','owned.png',?,?,?,?,1,0)`, []any{info.Size(), info.ModTime().UnixNano(), hash, stillFileIdentity(info)}},
		{`INSERT INTO media(id,album_id,source_entry_id,kind,file_name,mime,width,height,metadata_json,current_revision_id,created_at,updated_at) VALUES('input-media','input-album','input-entry','image','owned.png','image/png',3,2,?,'input-revision',0,0)`, []any{string(data)}},
		{`INSERT INTO media_revisions(id,media_id,source_sha256,source_size,source_mtime_ns,version,created_at) VALUES('input-revision','input-media',?,?,?,1,0)`, []any{hash, info.Size(), info.ModTime().UnixNano()}},
	}
	for _, query := range queries {
		if _, err := db.Exec(query.query, query.args...); err != nil {
			t.Fatal(err)
		}
	}
	q, err := jobs.New(db, jobs.DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	key, err := stillInputWorkKey("input-media", "input-revision", "thumb_320")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	job, _, err := q.EnqueueTx(t.Context(), tx, jobs.Input{Kind: "variant_still", ResourceKey: key, Dedupe: key, Payload: json.RawMessage(`{"version":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{"media:input-media", "source:input-source"} {
		if err := q.BindResourceTx(t.Context(), tx, job, resource); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	lease, err := q.Claim(t.Context(), "private-input-test", []string{"variant_still"})
	if err != nil {
		t.Fatal(err)
	}
	return stillInputFixture{New(db, nil, "", map[string]SourceRoot{"owned": configured}), q, lease, db, root, file, encoded.Bytes()}
}

func (f stillInputFixture) prepare(t *testing.T) (*preparedStillInput, error) {
	t.Helper()
	return f.svc.prepareStillInput(t.Context(), f.queue, f.lease, "input-media", "input-revision", "thumb_320", 1<<20)
}

func inputMutationCounts(t *testing.T, db *sql.DB) [5]int64 {
	t.Helper()
	var counts [5]int64
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM media_variants),(SELECT COUNT(*) FROM change_log),(SELECT COUNT(*) FROM outbox_events),(SELECT COUNT(*) FROM audit_events),(SELECT COALESCE(SUM(total),0) FROM jobs)`).Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestPreparedStillInputPinsReadOnlyRevisionWithoutPublishingOrGrantingPixels(t *testing.T) {
	f := newStillInputFixture(t)
	before := inputMutationCounts(t, f.db)
	prepared, err := f.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	data, err := io.ReadAll(prepared.input.file)
	if err != nil || !bytes.Equal(data, f.data) {
		t.Fatal("prepared descriptor is not exact source bytes", err)
	}
	if _, err := prepared.input.file.Write([]byte("no")); err == nil {
		t.Fatal("prepared source descriptor is writable")
	}
	if err := prepared.recheck(t.Context(), f.svc, f.queue, f.lease); err != nil {
		t.Fatal(err)
	}
	position, err := prepared.input.file.Seek(0, io.SeekCurrent)
	if err != nil || position != 0 {
		t.Fatal("verified input did not rewind for a producer", position, err)
	}
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.requireTx(t.Context(), f.svc, tx, f.queue, f.lease); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Rollback()
	if inputMutationCounts(t, f.db) != before {
		t.Fatal("read-only preparation published READY/progress/business state")
	}
	serialized, _ := json.Marshal(prepared)
	for _, secret := range []string{f.root, "owned.png", ".marker", prepared.snapshot.hash, f.lease.Owner} {
		if strings.Contains(string(serialized)+fmt.Sprint(prepared), secret) {
			t.Fatal("source capsule serialized private source/lease data", secret)
		}
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if err := prepared.recheck(t.Context(), f.svc, f.queue, f.lease); !errors.Is(err, errStillInput) {
		t.Fatal("closed capsule was reusable", err)
	}
	if data, err := os.ReadFile(f.file); err != nil || !bytes.Equal(data, f.data) {
		t.Fatal("preparation changed source", err)
	}
}

func TestPreparedStillInputWorkIdentityIsFiniteBoundedAndUnambiguous(t *testing.T) {
	base, err := stillInputWorkKey("input-media", "input-revision", "thumb_320")
	if err != nil || !strings.HasPrefix(base, "variant_still:") || !validHash(strings.TrimPrefix(base, "variant_still:")) {
		t.Fatal("invalid canonical work identity", base, err)
	}
	repeat, err := stillInputWorkKey("input-media", "input-revision", "thumb_320")
	if err != nil || repeat != base {
		t.Fatal("work identity is not deterministic", err)
	}
	for _, args := range [][3]string{
		{"other-media", "input-revision", "thumb_320"},
		{"input-media", "other-revision", "thumb_320"},
		{"input-media", "input-revision", "thumb_720"},
		{"input-media", "input-revision", "display_1920"},
	} {
		key, err := stillInputWorkKey(args[0], args[1], args[2])
		if err != nil || key == base {
			t.Fatal("distinct target coalesced into another work identity", args, err)
		}
	}
	for _, args := range [][3]string{
		{"", "input-revision", "thumb_320"},
		{"../media", "input-revision", "thumb_320"},
		{"media/child", "input-revision", "thumb_320"},
		{"media\x00", "input-revision", "thumb_320"},
		{"media\n", "input-revision", "thumb_320"},
		{strings.Repeat("m", 65), "input-revision", "thumb_320"},
		{"media", "", "thumb_320"},
		{"media", "../revision", "thumb_320"},
		{"media", "revision/child", "thumb_320"},
		{"media", "revision\x00", "thumb_320"},
		{"media", "revision\r", "thumb_320"},
		{"media", strings.Repeat("r", 129), "thumb_320"},
		{"media", "revision", "thumb_320;command"},
	} {
		if _, err := stillInputWorkKey(args[0], args[1], args[2]); !errors.Is(err, errStillInput) {
			t.Fatal("unsafe work identity accepted", args, err)
		}
	}
}

func TestPreparedStillInputRequiresCurrentIndexedScopeRevisionEpochAndLiveLease(t *testing.T) {
	for name, query := range map[string]string{
		"inactive source":        `UPDATE sources SET is_active=0 WHERE id='input-source'`,
		"inactive library":       `UPDATE libraries SET is_active=0 WHERE id='input-library'`,
		"configuration mismatch": `UPDATE sources SET config_version=config_version+1 WHERE id='input-source'`,
		"unhealthy source":       `UPDATE sources SET state='unavailable' WHERE id='input-source'`,
		"unavailable media":      `UPDATE media SET availability='unavailable' WHERE id='input-media'`,
		"entry unavailable":      `UPDATE source_entries SET availability='unavailable' WHERE id='input-entry'`,
		"missing identity":       `UPDATE source_entries SET file_identity='' WHERE id='input-entry'`,
		"missing root pin":       `UPDATE sources SET root_config_sha256='' WHERE id='input-source'`,
		"revision mismatch":      `UPDATE source_entries SET size_bytes=size_bytes+1 WHERE id='input-entry'`,
		"bad metadata":           `UPDATE media SET metadata_json='{"kind":"image","status":"invalid"}' WHERE id='input-media'`,
		"wrong media binding":    `DELETE FROM job_resources WHERE resource_key='media:input-media'`,
		"wrong source binding":   `DELETE FROM job_resources WHERE resource_key='source:input-source'`,
		"wrong kind":             `UPDATE jobs SET kind='scan' WHERE kind='variant_still'`,
		"wrong resource":         `UPDATE jobs SET resource_key='unrelated' WHERE kind='variant_still'`,
		"wrong payload version":  `UPDATE jobs SET payload_json='{"version":2}' WHERE kind='variant_still'`,
		"expired lease":          `UPDATE jobs SET lease_expires_at=0 WHERE kind='variant_still'`,
		"replacement owner":      `UPDATE jobs SET lease_owner='replacement' WHERE kind='variant_still'`,
		"cancelled lease":        `UPDATE jobs SET cancel_requested=1 WHERE kind='variant_still'`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newStillInputFixture(t)
			if _, err := f.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			before := inputMutationCounts(t, f.db)
			prepared, err := f.prepare(t)
			if prepared != nil {
				prepared.Close()
			}
			if err == nil {
				t.Fatal("unfenced source was prepared")
			}
			if inputMutationCounts(t, f.db) != before {
				t.Fatal("rejected preparation mutated published state")
			}
		})
	}
}

func TestPreparedStillInputDetectsRewrittenNamedFileMarkerRootAndLateDatabaseChanges(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, stillInputFixture){
		"same-stat rewrite": func(t *testing.T, f stillInputFixture) {
			info, err := os.Stat(f.file)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f.file, 0o600); err != nil {
				t.Fatal(err)
			}
			changed := append([]byte(nil), f.data...)
			changed[len(changed)/2] ^= 1
			if err := os.WriteFile(f.file, changed, 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(f.file, 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(f.file, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		},
		"equal-byte inode replacement": func(t *testing.T, f stillInputFixture) {
			info, err := os.Stat(f.file)
			if err != nil {
				t.Fatal(err)
			}
			replacement := f.file + "-replacement"
			if err := os.WriteFile(replacement, f.data, 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, f.file); err != nil {
				t.Fatal(err)
			}
		},
		"marker removal": func(t *testing.T, f stillInputFixture) {
			if err := os.Rename(filepath.Join(f.root, ".marker"), filepath.Join(f.root, ".offline")); err != nil {
				t.Fatal(err)
			}
		},
		"root replacement": func(t *testing.T, f stillInputFixture) {
			if err := os.Rename(f.root, f.root+"-offline"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(f.root, 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"indexed version": func(t *testing.T, f stillInputFixture) {
			if _, err := f.db.Exec(`UPDATE source_entries SET version=version+1 WHERE id='input-entry'`); err != nil {
				t.Fatal(err)
			}
		},
		"media ownership": func(t *testing.T, f stillInputFixture) {
			if _, err := f.db.Exec(`UPDATE media_revisions SET media_id='missing' WHERE id='input-revision'`); err == nil {
				t.Fatal("fixture expected FK denial")
			}
			if _, err := f.db.Exec(`UPDATE media SET current_revision_id=NULL WHERE id='input-media'`); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newStillInputFixture(t)
			prepared, err := f.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			mutate(t, f)
			if err := prepared.recheck(t.Context(), f.svc, f.queue, f.lease); err == nil {
				t.Fatal("changed source survived revalidation")
			}
		})
	}
}

func TestPreparedStillInputDBOnlyPublicationGuardRejectsLateStateChanges(t *testing.T) {
	for name, query := range map[string]string{
		"inactive source":       `UPDATE sources SET is_active=0 WHERE id='input-source'`,
		"inactive library":      `UPDATE libraries SET is_active=0 WHERE id='input-library'`,
		"source epoch":          `UPDATE sources SET config_version=config_version+1 WHERE id='input-source'`,
		"entry version":         `UPDATE source_entries SET version=version+1 WHERE id='input-entry'`,
		"media version":         `UPDATE media SET version=version+1 WHERE id='input-media'`,
		"current revision":      `UPDATE media SET current_revision_id=NULL WHERE id='input-media'`,
		"revision version":      `UPDATE media_revisions SET version=version+1 WHERE id='input-revision'`,
		"immutable metadata":    `UPDATE media SET metadata_json='{}' WHERE id='input-media'`,
		"source dependency":     `DELETE FROM job_resources WHERE resource_key='source:input-source'`,
		"different dedupe":      `UPDATE jobs SET dedupe_token='different' WHERE kind='variant_still'`,
		"different attempt":     `UPDATE jobs SET attempts=attempts+1 WHERE kind='variant_still'`,
		"different lease owner": `UPDATE jobs SET lease_owner='replacement' WHERE kind='variant_still'`,
		"expired lease":         `UPDATE jobs SET lease_expires_at=0 WHERE kind='variant_still'`,
		"cancelled lease":       `UPDATE jobs SET cancel_requested=1 WHERE kind='variant_still'`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newStillInputFixture(t)
			prepared, err := f.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Close()
			if _, err := f.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			before := inputMutationCounts(t, f.db)
			tx, err := f.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			err = prepared.requireTx(t.Context(), f.svc, tx, f.queue, f.lease)
			tx.Rollback()
			if err == nil {
				t.Fatal("late state change survived the DB-only publication fence")
			}
			if inputMutationCounts(t, f.db) != before {
				t.Fatal("rejected publication guard mutated business/progress/READY rows")
			}
		})
	}
}

func TestPreparedStillInputRequiresFullHashEvenWhenIndexedStatAndInodeMatch(t *testing.T) {
	f := newStillInputFixture(t)
	info, err := os.Stat(f.file)
	if err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), f.data...)
	changed[len(changed)/2] ^= 1
	if err := os.Chmod(f.file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.file, changed, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.file, 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.file, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(f.file)
	if err != nil || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		t.Fatal("fixture did not preserve indexed stat/inode", err)
	}
	prepared, err := f.prepare(t)
	if prepared != nil {
		prepared.Close()
	}
	if !errors.Is(err, errStillInput) {
		t.Fatal("same-stat overwritten bytes were trusted without full SHA", err)
	}
}

func TestPreparedStillInputRefusesMissingRootsMarkersAliasesAndEscapingNames(t *testing.T) {
	for _, name := range []string{"missing-root", "missing-marker", "root-alias", "escaping-file", "directory-file"} {
		t.Run(name, func(t *testing.T) {
			f := newStillInputFixture(t)
			switch name {
			case "missing-root":
				if err := os.Rename(f.root, f.root+"-offline"); err != nil {
					t.Fatal(err)
				}
			case "missing-marker":
				if err := os.Remove(filepath.Join(f.root, ".marker")); err != nil {
					t.Fatal(err)
				}
			case "root-alias":
				if err := os.Rename(f.root, f.root+"-target"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.root+"-target", f.root); err != nil {
					t.Fatal(err)
				}
			case "escaping-file":
				outside := filepath.Join(t.TempDir(), "private.png")
				if err := os.WriteFile(outside, f.data, 0o400); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(f.file); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, f.file); err != nil {
					t.Fatal(err)
				}
			case "directory-file":
				if err := os.Remove(f.file); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(f.file, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			prepared, err := f.prepare(t)
			if prepared != nil {
				prepared.Close()
			}
			if err == nil {
				t.Fatal("unsafe source was prepared")
			}
			if name == "missing-root" {
				if _, err := os.Lstat(f.root); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("preparation recreated missing source", err)
				}
			}
			if name == "missing-marker" {
				if _, err := os.Lstat(filepath.Join(f.root, ".marker")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("preparation recreated missing marker", err)
				}
			}
		})
	}
}

func TestPreparedStillInputBudgetCancellationAndWrongLeaseIdentity(t *testing.T) {
	f := newStillInputFixture(t)
	for _, budget := range []int64{0, int64(len(f.data) - 1), 1<<40 + 1} {
		if input, err := f.svc.prepareStillInput(t.Context(), f.queue, f.lease, "input-media", "input-revision", "thumb_320", budget); err == nil {
			input.Close()
			t.Fatal("invalid input-byte budget accepted", budget)
		}
	}
	for _, preset := range []string{"fixture_png", "thumb_320;command", "unknown"} {
		if _, err := stillInputWorkKey("input-media", "input-revision", preset); err == nil {
			t.Fatal("non-finite work preset accepted", preset)
		}
	}
	prepared, err := f.prepare(t)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	wrong := f.lease
	wrong.Owner = "other-owner"
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.requireTx(t.Context(), f.svc, tx, f.queue, wrong); !errors.Is(err, errStillInput) {
		tx.Rollback()
		t.Fatal("capsule transferred to a different lease", err)
	}
	tx.Rollback()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.svc.prepareStillInput(ctx, f.queue, f.lease, "input-media", "input-revision", "thumb_320", 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled preparation continued", err)
	}
	if err := prepared.recheck(ctx, f.svc, f.queue, f.lease); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled recheck continued", err)
	}
}

func TestPreparedStillInputReleasesWriterDuringStreamedHashAndRejectsLateEpoch(t *testing.T) {
	f := newStillInputFixture(t)
	// Fixture-only sparse bytes exercise streaming/transaction behavior, not
	// additional PNG validity or decoded-pixel evidence.
	if err := os.Chmod(f.file, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(f.file, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	const size = int64(512 << 20)
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := os.Chmod(f.file, 0o400); err != nil {
		t.Fatal(err)
	}
	read, err := os.Open(f.file)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, read); err != nil {
		t.Fatal(err)
	}
	read.Close()
	info, err := os.Stat(f.file)
	if err != nil {
		t.Fatal(err)
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	if _, err := f.db.Exec(`UPDATE source_entries SET size_bytes=?,mtime_ns=?,sha256=? WHERE id='input-entry'`, size, info.ModTime().UnixNano(), hash); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE media_revisions SET source_size=?,source_mtime_ns=?,source_sha256=? WHERE id='input-revision'`, size, info.ModTime().UnixNano(), hash); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		prepared *preparedStillInput
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		prepared, err := f.svc.prepareStillInput(t.Context(), f.queue, f.lease, "input-media", "input-revision", "thumb_320", size)
		done <- outcome{prepared, err}
	}()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	observed := false
	for !observed {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if target == f.file {
				observed = true
				break
			}
		}
		if observed {
			break
		}
		select {
		case result := <-done:
			if result.prepared != nil {
				result.prepared.Close()
			}
			t.Fatal("source hash finished before its IO checkpoint", result.err)
		case <-deadline.C:
			t.Fatal("source descriptor was not opened")
		case <-tick.C:
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("streaming source hash held the SQLite writer", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE source_entries SET version=version+1 WHERE id='input-entry'`); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.prepared != nil {
			result.prepared.Close()
		}
		if !errors.Is(result.err, errStillInput) {
			t.Fatal("late indexed epoch was not rejected after IO", result.err)
		}
	case <-deadline.C:
		t.Fatal("streamed source verification did not return")
	}
}
