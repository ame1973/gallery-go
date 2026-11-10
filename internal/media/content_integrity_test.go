package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestContentDigestVerificationRewindsAndRejectsMismatches(t *testing.T) {
	data := bytes.Repeat([]byte("bounded-content"), 100000)
	name := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	content := &Content{file: file, info: info, Size: info.Size(), ctx: t.Context()}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if err := content.verifyDigest(hash); err != nil {
		t.Fatal("matching digest rejected", err)
	}
	got, err := io.ReadAll(content)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("verification did not rewind the unchanged representation", len(got), err)
	}
	if err := content.verifyDigest(string(bytes.Repeat([]byte{'0'}, 64))); !errors.Is(err, ErrContentChanged) {
		t.Fatal("incorrect digest accepted", err)
	}
	content.Size++
	if err := content.verifyDigest(hash); !errors.Is(err, ErrContentChanged) {
		t.Fatal("short representation accepted", err)
	}
	content.Size--
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	content.ctx = ctx
	if err := content.verifyDigest(hash); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled verification continued", err)
	}
	if err := content.sameNamedFile(nil, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled name recheck attempted filesystem I/O", err)
	}
	if _, err := readContentMarker(ctx, nil, "unused"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled marker recheck attempted filesystem I/O", err)
	}
}

func TestContentNamedFileDetectsReplacementAndRemoval(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "input")
	if err := os.WriteFile(name, []byte("indexed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	file, info, err := OpenReadOnlyFile(root, "input")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content := &Content{file: file, info: info, Size: info.Size(), ctx: t.Context()}
	if err := content.sameNamedFile(root, "input"); err != nil {
		t.Fatal("unchanged rooted name rejected", err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, []byte("indexed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, name); err != nil {
		t.Fatal(err)
	}
	if err := content.sameNamedFile(root, "input"); !errors.Is(err, ErrContentChanged) {
		t.Fatal("replacement redirected the verified descriptor", err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := content.sameNamedFile(root, "input"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("removed rooted name remained valid", err)
	}
}

func TestContentVerificationSlotsBoundWorkAndHonorCancellation(t *testing.T) {
	s := New(nil, nil, "", nil)
	for range cap(s.contentVerifies) {
		if err := s.acquireContentVerification(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		done <- s.acquireContentVerification(ctx)
	}()
	<-started
	select {
	case err := <-done:
		t.Fatal("verification exceeded the per-service bound", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("waiting verification ignored cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled verification remained blocked")
	}
	if len(s.contentVerifies) != cap(s.contentVerifies) {
		t.Fatal("cancelled waiter consumed or released another verification slot")
	}
	<-s.contentVerifies
	if err := s.acquireContentVerification(t.Context()); err != nil {
		t.Fatal("released verification slot was not reusable", err)
	}
}

func TestContentOpenVerificationWaitsWithRequestCancellation(t *testing.T) {
	f := fixture(t)
	for range cap(f.svc.contentVerifies) {
		if err := f.svc.acquireContentVerification(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, variant := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		var content *Content
		var err error
		if variant {
			content, err = f.svc.OpenVariant(ctx, f.reader, f.mediaID, "variant")
		} else {
			content, err = f.svc.OpenOriginal(ctx, f.reader, f.mediaID)
		}
		cancel()
		if content != nil {
			content.Close()
		}
		if !errors.Is(err, context.DeadlineExceeded) || len(f.svc.contentVerifies) != cap(f.svc.contentVerifies) {
			t.Fatal("cancelled open bypassed the verification bound or disturbed another slot", variant, err)
		}
	}
	for range cap(f.svc.contentVerifies) {
		<-f.svc.contentVerifies
	}
	for _, variant := range []bool{false, true} {
		var content *Content
		var err error
		if variant {
			content, err = f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
		} else {
			content, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
		}
		if err != nil {
			t.Fatal("released verification slots were not reusable", variant, err)
		}
		content.Close()
		if len(f.svc.contentVerifies) != 0 {
			t.Fatal("successful open leaked a verification slot", variant)
		}
	}
}

func TestContentOpenHashMismatchReleasesVerificationSlot(t *testing.T) {
	f := fixture(t)
	for _, variant := range []bool{false, true} {
		filename, want := f.original, ErrSourceUnavailable
		if variant {
			filename, want = f.variant, ErrVariantUnavailable
		}
		before, err := os.Stat(filename)
		if err != nil {
			t.Fatal(err)
		}
		changed := bytes.Clone(f.data)
		changed[len(changed)/2] ^= 1
		if err := os.WriteFile(filename, changed, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filename, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
		var content *Content
		if variant {
			content, err = f.svc.OpenVariant(t.Context(), f.reader, f.mediaID, "variant")
		} else {
			content, err = f.svc.OpenOriginal(t.Context(), f.reader, f.mediaID)
		}
		if content != nil {
			content.Close()
		}
		if !errors.Is(err, want) || len(f.svc.contentVerifies) != 0 {
			t.Fatal("hash mismatch was accepted or leaked a verification slot", variant, err)
		}
		if err := os.WriteFile(filename, f.data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filename, before.ModTime(), before.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContentStateRechecksVersionsVisibilityAndOriginalPolicy(t *testing.T) {
	f := fixture(t)
	original, err := f.svc.originalContentState(t.Context(), f.reader, f.mediaID)
	if err != nil {
		t.Fatal(err)
	}
	variant, err := f.svc.variantContentState(t.Context(), f.reader, f.mediaID, "variant")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.requireOriginalContentState(t.Context(), f.reader, f.mediaID, original); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.requireVariantContentState(t.Context(), f.reader, f.mediaID, "variant", variant); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE media SET version=version+1 WHERE id=?`, f.mediaID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.requireOriginalContentState(t.Context(), f.reader, f.mediaID, original); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatal("changed media epoch retained the original state", err)
	}
	if err := f.svc.requireVariantContentState(t.Context(), f.reader, f.mediaID, "variant", variant); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("changed media epoch retained the variant state", err)
	}
	if _, err := f.db.Exec(`UPDATE media SET version=version-1 WHERE id=?`, f.mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE sources SET state='unavailable' WHERE id='source'`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.requireOriginalContentState(t.Context(), f.reader, f.mediaID, original); !errors.Is(err, ErrSourceUnavailable) {
		t.Fatal("source outage retained the original state", err)
	}
	if err := f.svc.requireVariantContentState(t.Context(), f.reader, f.mediaID, "variant", variant); err != nil {
		t.Fatal("source outage hid an otherwise current cached variant", err)
	}
	if _, err := f.db.Exec(`UPDATE sources SET state='healthy' WHERE id='source'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE users SET download_original=0 WHERE id=?`, f.reader.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.requireOriginalContentState(t.Context(), f.reader, f.mediaID, original); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("revoked original policy survived the post-hash check", err)
	}
	if err := f.svc.requireVariantContentState(t.Context(), f.reader, f.mediaID, "variant", variant); err != nil {
		t.Fatal("original policy incorrectly controls cached previews", err)
	}
	if _, err := f.db.Exec(`DELETE FROM user_groups WHERE user_id=?`, f.reader.User.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.requireVariantContentState(t.Context(), f.reader, f.mediaID, "variant", variant); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("revoked grant survived the post-hash check", err)
	}
}
