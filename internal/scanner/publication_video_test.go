//go:build linux && (amd64 || arm64)

package scanner

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"gallerygo/internal/media"
)

func publicationVideo(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/video.json")
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]string
	if json.Unmarshal(data, &encoded) != nil {
		t.Fatal("invalid owned video vectors")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded["mov"])
	if err != nil || len(decoded) == 0 {
		t.Fatal("missing owned MOV vector", err)
	}
	return decoded
}

func TestVideoPublicationPreservesEvidenceAndClearsItOnMagicRewrite(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	original := publicationVideo(t)
	path := writeInput(t, root, "Album/clip.mov", original)
	generation := f.publish(t, "full", ".")
	var id, revision string
	var version int64
	if err := f.db.QueryRow(`SELECT id,current_revision_id,version FROM media`).Scan(&id, &revision, &version); err != nil {
		t.Fatal(err)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	item, err := service.Get(t.Context(), f.owner, id)
	if err != nil || item.Kind != "video" || item.Codec == nil || *item.Codec != "h264" || item.Width == nil || *item.Width != 16 || item.Height == nil || *item.Height != 12 || item.DurationMS == nil || *item.DurationMS != 400 || item.Orientation == nil || *item.Orientation != 6 || item.CaptureAt == nil || item.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" || item.Animated != nil || item.FrameCount != nil || item.LoopCount != nil {
		t.Fatal("video metadata was not source-bound and atomically published", item, err)
	}
	job, err := f.queue.Get(t.Context(), generation.JobID)
	if err != nil || job.Status != "succeeded" || job.Succeeded != 3 || job.Failed != 0 || job.Total != 3 || !job.TotalKnown {
		t.Fatal("video work counters were invented", job, err)
	}
	f.publish(t, "incremental", ".")
	var repeatedRevision string
	var repeatedVersion int64
	if err := f.db.QueryRow(`SELECT current_revision_id,version FROM media WHERE id=?`, id).Scan(&repeatedRevision, &repeatedVersion); err != nil || repeatedRevision != revision || repeatedVersion != version {
		t.Fatal("unchanged video duplicated logical revision/version", err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, original) {
		t.Fatal("video metadata processing rewrote source bytes", err)
	}
	replacement := writeInput(t, root, "replacement.png", metadataPNG(t))
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	changed, err := service.Get(t.Context(), f.owner, id)
	if err != nil || changed.Kind != "image" || changed.Codec == nil || *changed.Codec != "png" || changed.Orientation != nil || changed.CaptureAt != nil || changed.DurationMS != nil || changed.Revision == nil || *changed.Revision == revision || changed.Animated == nil || *changed.Animated || changed.FrameCount == nil || *changed.FrameCount != 1 {
		t.Fatal("magic rewrite retained stale video codec/clock/transform", changed, err)
	}
	var owner string
	if err := f.db.QueryRow(`SELECT media_id FROM media_revisions WHERE id=?`, revision).Scan(&owner); err != nil || owner != id {
		t.Fatal("video rewrite discarded owned history", owner, err)
	}
}

func TestMetadataRuntimeEpochCannotChangeWithinAnInspectorVersion(t *testing.T) {
	f := inventorySetup(t)
	writeInput(t, media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}, "Album/clip.mov", publicationVideo(t))
	lease, generation := f.ready(t)
	policy := media.DefaultInspectPolicy()
	generation, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(struct {
		Version      int
		Policy       media.InspectPolicy
		CodecRuntime string
	}{inspectVersion, policy, strings.Repeat("0", 64)})
	digest := sha256.Sum256(data)
	if _, err := f.db.Exec(`UPDATE inventory_generations SET inspect_policy_sha256=? WHERE id=?`, hex.EncodeToString(digest[:]), generation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, policy); !errors.Is(err, ErrGeneration) {
		t.Fatal("installation epochs mixed inside one metadata generation", err)
	}
	after, err := f.queue.Get(t.Context(), lease.Job.ID)
	if err != nil || before.Counts != after.Counts {
		t.Fatal("rejected runtime epoch duplicated progress", before, after, err)
	}
	var indexed int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM media`).Scan(&indexed); err != nil || indexed != 0 {
		t.Fatal("rejected runtime epoch published media", indexed, err)
	}
}

func TestMetadataCodecEvidenceIsBoundedAndMandatoryForCurrentSuccess(t *testing.T) {
	width, height, codec := int64(16), int64(12), "h264"
	valid := media.Inspection{Kind: "video", MIME: "video/mp4", Format: "mp4", Codec: &codec, Width: &width, Height: &height, Status: "ok"}
	if !validInspection(valid) {
		t.Fatal("valid codec evidence rejected")
	}
	for _, text := range []string{"", "unknown", "GPS private tag", "/private/runtime", strings.Repeat("x", 65)} {
		invalid := valid
		invalid.Codec = &text
		if validInspection(invalid) {
			t.Fatal("unsafe codec evidence entered immutable staging", text)
		}
	}
	valid.Codec = nil
	if validInspection(valid) {
		t.Fatal("current successful inspection omitted the codec")
	}
}
