package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gallerygo/internal/auth"
)

func TestIssuePagesRequireFreshAdminAndProjectOnlySafeHistoricalContext(t *testing.T) {
	q, _ := fixture(t)
	admin, owner := adminFixture(t, q)
	job := enqueued(t, q, "diagnostics", 0)
	other := enqueued(t, q, "another-job", 0)
	for _, issue := range []struct{ id, job, generation, path, code string }{
		{"first", job.ID, "old-generation", "Album/bad.jpg", "INVALID_MEDIA"},
		{"second", job.ID, "", ".", "SOURCE_UNAVAILABLE"},
		{"third", job.ID, "old-generation", "/private/source/marker", "/private/stderr/worker-secret"},
		{"other", other.ID, "", "Another/private.jpg", "IO_ERROR"},
	} {
		if _, err := q.db.Exec(`INSERT INTO job_issues VALUES(?,?,1,'historical-source',7,?,?,?,?)`, issue.id, issue.job, issue.generation, issue.path, issue.code, q.clock().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	first, err := admin.Issues(t.Context(), owner, job.ID, "", 1)
	if err != nil || len(first) != 1 || first[0].ID != "first" || first[0].Attempt != 1 || first[0].SourceID != "historical-source" || first[0].SourceConfigVersion != 7 || first[0].GenerationID == nil || *first[0].GenerationID != "old-generation" || first[0].RelativePath == nil || *first[0].RelativePath != "Album/bad.jpg" || first[0].ErrorCode != "INVALID_MEDIA" || first[0].Message != failureMessages["INVALID_MEDIA"] || !first[0].CreatedAt.Equal(q.clock()) {
		t.Fatal("diagnostic history or stable bounded page was lost", first, err)
	}
	rest, err := admin.Issues(t.Context(), owner, job.ID, first[0].ID, 101)
	if err != nil || len(rest) != 2 || rest[0].ID != "second" || rest[0].GenerationID != nil || rest[0].RelativePath == nil || *rest[0].RelativePath != "." || rest[1].ID != "third" || rest[1].RelativePath != nil || rest[1].ErrorCode != "PROCESSING_FAILED" {
		t.Fatal("root/unsafe/unknown issues were guessed or mixed across jobs", rest, err)
	}
	data, err := json.Marshal(rest)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"/private", "stderr", "worker-secret", "payload_json", "lease_owner", "root_path", `"marker"`, "fingerprint"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("private diagnostic data leaked", secret, string(data))
		}
	}
	empty, err := admin.Issues(t.Context(), owner, job.ID, "z", 30)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatal("empty issue page is not an explicit empty collection", empty, err)
	}
	for _, limit := range []int{0, 102} {
		_, err := admin.Issues(t.Context(), owner, job.ID, "", limit)
		requireError(t, err, ErrInvalidJob)
	}
	_, err = admin.Issues(t.Context(), owner, "missing", "", 30)
	requireError(t, err, sql.ErrNoRows)
	_, err = admin.Issues(t.Context(), auth.Session{}, job.ID, "", 30)
	requireError(t, err, auth.ErrUnauthenticated)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := admin.Issues(ctx, owner, job.ID, "", 30); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled diagnostic query continued", err)
	}
	if _, err := q.db.Exec(`UPDATE users SET role='user' WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	_, err = admin.Issues(t.Context(), owner, job.ID, "", 30)
	requireError(t, err, auth.ErrForbidden)
	if _, err := q.db.Exec(`UPDATE users SET role='admin',active=0 WHERE id=?`, owner.User.ID); err != nil {
		t.Fatal(err)
	}
	_, err = admin.Issues(t.Context(), owner, job.ID, "", 30)
	requireError(t, err, auth.ErrUnauthenticated)
}

func TestIssuePathNeverRewritesUnsafeOrUnrepresentableNames(t *testing.T) {
	for _, path := range []string{".", "Album/photo.jpg", "相簿/照片.jpg", "Album/a b.jpg", "Album/photo:one.jpg", "Album/<picture>.jpg"} {
		value := issuePath(path)
		if value == nil || *value != path {
			t.Fatal("safe relative path was guessed or discarded", path, value)
		}
	}
	for _, path := range []string{"", "/private/root", "../private", "Album/../../root", "Album//file", "Album/./file", `Album\file`, "C:/private/root", "//server/share", "https://private.invalid/root", "Album/\x00file", "Album/\nfile", "Album/\u202efile", "Album/\xfffile", strings.Repeat("x", 4097)} {
		if value := issuePath(path); value != nil {
			t.Fatal("unsafe diagnostic path exposed or normalized", path, *value)
		}
	}
	for code, message := range issueMessages {
		actualCode, actualMessage := issueDescription(code)
		if actualCode != code || actualMessage != message || message == "" {
			t.Fatal("known issue code lost its fixed message", code)
		}
	}
}

func FuzzSafeIssueProjection(f *testing.F) {
	for _, value := range []string{"Album/bad.jpg", ".", "/private/root", "UNSAFE_ENTRY", "CODEC_UNAVAILABLE", "C:/private/root", "a\xff.jpg"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 8192 {
			return
		}
		path := issuePath(value)
		if path != nil && (*path != value || len(*path) > 4096) {
			t.Fatal("path projection guessed or expanded private input")
		}
		code, message := issueDescription(value)
		if message == "" || code != value && code != "PROCESSING_FAILED" {
			t.Fatal("unknown diagnostic code became raw detail", code, message)
		}
	})
}
