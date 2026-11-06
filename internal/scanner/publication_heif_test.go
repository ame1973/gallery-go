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

func publicationHEIF(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/heif.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if json.Unmarshal(data, &fixtures) != nil {
		t.Fatal("invalid owned HEIF vectors")
	}
	decoded, err := base64.StdEncoding.DecodeString(fixtures[name])
	if err != nil || len(decoded) == 0 {
		t.Fatal("missing owned HEIF vector", name, err)
	}
	return decoded
}

func TestHEIFPublicationRetainsPrimaryMetadataAndClearsFormatRewriteEvidence(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	inputs := map[string][]byte{
		"capture.jpg": publicationHEIF(t, "mirrored"), "primary.png": publicationHEIF(t, "collection"),
		"generic.gif": publicationHEIF(t, "generic"), "photo.png": metadataPNG(t),
	}
	paths := map[string]string{}
	for name, data := range inputs {
		paths[name] = writeInput(t, root, "Album/"+name, data)
	}
	generation := f.publish(t, "full", ".")
	job, err := f.queue.Get(t.Context(), generation.JobID)
	if err != nil || job.Status != "succeeded" || job.Total != 12 || job.Succeeded != 12 || !job.TotalKnown {
		t.Fatal("HEIF publication invented successful work", job, err)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	var album string
	if err := f.db.QueryRow(`SELECT id FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	items, err := service.AlbumMedia(t.Context(), f.owner, album, media.AlbumMediaFilter{}, media.AlbumMediaPosition{}, 30)
	if err != nil || len(items) != 4 {
		t.Fatal("HEIF list visibility diverged", items, err)
	}
	ids, revisions, snapshots := map[string]string{}, map[string]string{}, map[string]string{}
	for _, item := range items {
		if item.Revision == nil || item.Codec == nil || item.Width == nil || item.Height == nil {
			t.Fatal("source metadata not published", item)
		}
		if item.FileName != "photo.png" {
			width := int64(64)
			if item.FileName == "capture.jpg" {
				width = 96
				if item.Orientation == nil || *item.Orientation != 5 || item.CaptureAt == nil || item.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" {
					t.Fatal("HEIF transforms/primary EXIF lost", item)
				}
			} else if item.Orientation != nil || item.CaptureAt != nil {
				t.Fatal("HEIF unknown metadata was fabricated", item)
			}
			if item.Kind != "image" || *item.Codec != "hevc" || *item.Width != width || *item.Height != 64 || item.Animated == nil || *item.Animated || item.FrameCount == nil || *item.FrameCount != 1 || item.LoopCount == nil || *item.LoopCount != 1 || item.DurationMS != nil {
				t.Fatal("HEIF primary/grid evidence was not source-bound", item)
			}
		}
		encoded, _ := json.Marshal(item)
		if bytes.Contains(encoded, []byte(f.rootPath)) || bytes.Contains(encoded, []byte("hvcC")) || bytes.Contains(encoded, []byte("GPS")) {
			t.Fatal("private HEIF details escaped metadata DTO", string(encoded))
		}
		ids[item.FileName], revisions[item.FileName], snapshots[item.FileName] = item.ID, *item.Revision, string(encoded)
	}
	f.publish(t, "incremental", ".")
	for name, id := range ids {
		item, err := service.Get(t.Context(), f.owner, id)
		encoded, _ := json.Marshal(item)
		if err != nil || string(encoded) != snapshots[name] {
			t.Fatal("no-op HEIF rescan changed identity/metadata", item, err)
		}
		actual, readErr := os.ReadFile(paths[name])
		info, statErr := os.Stat(paths[name])
		if readErr != nil || statErr != nil || !bytes.Equal(actual, inputs[name]) || info.Mode().Perm() != 0o400 {
			t.Fatal("HEIF inspection wrote source bytes/mode", name, readErr, statErr)
		}
	}
	for name, data := range map[string][]byte{"capture.jpg": publicationTIFF(t, "plain"), "photo.png": publicationHEIF(t, "still")} {
		replacement := writeInput(t, root, "replacement", data)
		if err := os.Rename(replacement, paths[name]); err != nil {
			t.Fatal(err)
		}
	}
	f.publish(t, "full", ".")
	for name, codec := range map[string]string{"capture.jpg": "tiff", "photo.png": "hevc"} {
		item, err := service.Get(t.Context(), f.owner, ids[name])
		if err != nil || item.Codec == nil || *item.Codec != codec || item.CaptureAt != nil || item.Orientation != nil || item.DurationMS != nil || item.Revision == nil || *item.Revision == revisions[name] {
			t.Fatal("format rewrite retained HEIF codec/EXIF/revision", item, err)
		}
		if codec == "tiff" && (item.Animated != nil || item.FrameCount != nil || item.LoopCount != nil) {
			t.Fatal("document rewrite retained still-picture frame evidence", item)
		}
	}
	for name, revision := range revisions {
		var owner string
		if err := f.db.QueryRow(`SELECT media_id FROM media_revisions WHERE id=?`, revision).Scan(&owner); err != nil || owner != ids[name] {
			t.Fatal("HEIF rewrite lost owned revision history", owner, err)
		}
	}
}
