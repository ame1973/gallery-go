//go:build linux && (amd64 || arm64)

package scanner

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"

	"gallerygo/internal/media"
)

func publicationAVIF(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/avif.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if json.Unmarshal(data, &fixtures) != nil {
		t.Fatal("invalid owned AVIF vectors")
	}
	decoded, err := base64.StdEncoding.DecodeString(fixtures[name])
	if err != nil || len(decoded) == 0 {
		t.Fatal("missing owned AVIF vector", name, err)
	}
	return decoded
}

func TestAVIFPublicationRetainsIdentityAndClearsStaleContainerEvidence(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	still := publicationAVIF(t, "mirrored")
	animation := publicationAVIF(t, "infinite")
	stillPath := writeInput(t, root, "Album/still.jpg", still)
	animationPath := writeInput(t, root, "Album/motion.png", animation)
	grid := publicationAVIF(t, "grid")
	gridPath := writeInput(t, root, "Album/grid.jpg", grid)
	generic := publicationAVIF(t, "still")
	copy(generic[8:12], "mif1")
	genericPath := writeInput(t, root, "Album/generic.png", generic)
	generation := f.publish(t, "full", ".")
	job, err := f.queue.Get(t.Context(), generation.JobID)
	if err != nil || job.Status != "succeeded" || job.Succeeded != 12 || job.Total != 12 || !job.TotalKnown {
		var failures string
		_ = f.db.QueryRow(`SELECT json_group_array(json_object('path',relative_path,'inspection',json(inspection_json))) FROM inventory_media WHERE generation_id=? AND status!='ok'`, generation.ID).Scan(&failures)
		t.Fatalf("AVIF publication failed: job=%v counts=%+v error=%v media=%s", job, job.Counts, err, failures)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	ids, revisions, snapshots := map[string]string{}, map[string]string{}, map[string]string{}
	var album string
	rows, err := f.db.Query(`SELECT id,file_name,album_id,current_revision_id FROM media`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, name, revision string
		if err := rows.Scan(&id, &name, &album, &revision); err != nil {
			t.Fatal(err)
		}
		ids[name], revisions[name] = id, revision
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	items, err := service.AlbumMedia(t.Context(), f.owner, album, "", 30)
	if err != nil || len(items) != 4 {
		t.Fatal("AVIF list visibility diverged", items, err)
	}
	for _, item := range items {
		width, height := int64(16), int64(12)
		if item.FileName == "grid.jpg" {
			width, height = 96, 64
		}
		if item.Kind != "image" || item.Codec == nil || *item.Codec != "av1" || item.Width == nil || *item.Width != width || item.Height == nil || *item.Height != height || item.FrameCount == nil || item.LoopCount == nil || item.Animated == nil {
			t.Fatal("AVIF metadata was not source-bound", item)
		}
		if item.FileName == "still.jpg" {
			if *item.Animated || *item.FrameCount != 1 || *item.LoopCount != 1 || item.Orientation == nil || *item.Orientation != 5 || item.CaptureAt == nil || item.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" || item.DurationMS != nil {
				t.Fatal("AVIF transform/EXIF was lost", item)
			}
		} else if item.FileName == "grid.jpg" || item.FileName == "generic.png" {
			if *item.Animated || *item.FrameCount != 1 || *item.LoopCount != 1 || item.Orientation != nil || item.CaptureAt != nil || item.DurationMS != nil {
				t.Fatal("image profile invented source metadata", item)
			}
		} else if !*item.Animated || *item.FrameCount != 2 || *item.LoopCount != 0 || item.DurationMS == nil || *item.DurationMS != 400 || item.Orientation != nil || item.CaptureAt != nil {
			t.Fatal("AVIF loop normalization was guessed", item)
		}
		encoded, _ := json.Marshal(item)
		snapshots[item.FileName] = string(encoded)
	}
	f.publish(t, "incremental", ".")
	for name, id := range ids {
		item, err := service.Get(t.Context(), f.owner, id)
		encoded, _ := json.Marshal(item)
		if err != nil || string(encoded) != snapshots[name] {
			t.Fatal("unchanged AVIF revised metadata or identity", item, err)
		}
	}
	for path, expected := range map[string][]byte{stillPath: still, animationPath: animation, gridPath: grid, genericPath: generic} {
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, expected) {
			t.Fatal("AVIF inspector rewrote source", err)
		}
	}
	replacement := writeInput(t, root, "replacement.png", metadataPNG(t))
	if err := os.Rename(replacement, stillPath); err != nil {
		t.Fatal(err)
	}
	replacement = writeInput(t, root, "replacement.avif", publicationAVIF(t, "rotated"))
	if err := os.Rename(replacement, animationPath); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	stillItem, err := service.Get(t.Context(), f.owner, ids["still.jpg"])
	if err != nil || stillItem.Codec == nil || *stillItem.Codec != "png" || stillItem.CaptureAt != nil || stillItem.Orientation != nil || stillItem.DurationMS != nil || stillItem.Revision == nil || *stillItem.Revision == revisions["still.jpg"] {
		t.Fatal("magic rewrite retained AVIF metadata", stillItem, err)
	}
	static, err := service.Get(t.Context(), f.owner, ids["motion.png"])
	if err != nil || static.Codec == nil || *static.Codec != "av1" || static.Animated == nil || *static.Animated || static.FrameCount == nil || *static.FrameCount != 1 || static.LoopCount == nil || *static.LoopCount != 1 || static.DurationMS != nil || static.Revision == nil || *static.Revision == revisions["motion.png"] {
		t.Fatal("static AVIF retained old sequence timing", static, err)
	}
	for name, revision := range revisions {
		var owner string
		if err := f.db.QueryRow(`SELECT media_id FROM media_revisions WHERE id=?`, revision).Scan(&owner); err != nil || owner != ids[name] {
			t.Fatal("AVIF rewrite lost owned revision history", owner, err)
		}
	}
}
