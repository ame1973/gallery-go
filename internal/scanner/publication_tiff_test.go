//go:build linux && (amd64 || arm64)

package scanner

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
	"time"

	"gallerygo/internal/media"
)

func publicationTIFF(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/tiff.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]string
	if json.Unmarshal(data, &fixtures) != nil {
		t.Fatal("invalid owned TIFF vectors")
	}
	decoded, err := base64.StdEncoding.DecodeString(fixtures[name])
	if err != nil || len(decoded) == 0 {
		t.Fatal("missing owned TIFF vector", name, err)
	}
	return decoded
}

func TestTIFFPublicationPreservesIdentityAndClearsRewrittenEXIFOrAnimation(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	inputs, paths := map[string][]byte{}, map[string]string{}
	for _, vector := range []string{"little_exif", "big_exif", "multipage", "deflate", "plain", "tiled_deflate", "strips"} {
		name := vector + ".jpg"
		inputs[name] = publicationTIFF(t, vector)
		paths[name] = writeInput(t, root, "Album/"+name, inputs[name])
	}
	inputs["photo.png"] = metadataPNG(t)
	paths["photo.png"] = writeInput(t, root, "Album/photo.png", inputs["photo.png"])
	generation := f.publish(t, "full", ".")
	job, err := f.queue.Get(t.Context(), generation.JobID)
	if err != nil || job.Status != "succeeded" || job.Succeeded != 24 || job.Total != 24 || !job.TotalKnown {
		t.Fatal("TIFF publication invented progress", job, err)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	var album string
	if err := f.db.QueryRow(`SELECT id FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	items, err := service.AlbumMedia(t.Context(), f.owner, album, "", 30)
	if err != nil || len(items) != len(inputs) {
		t.Fatal("TIFF list visibility diverged", items, err)
	}
	ids, revisions, snapshots := map[string]string{}, map[string]string{}, map[string]string{}
	for _, item := range items {
		if item.Kind != "image" || item.Codec == nil || item.Width == nil || *item.Width != 12 || item.Height == nil || *item.Height != 7 || item.Revision == nil {
			t.Fatal("source-bound TIFF metadata missing", item)
		}
		if item.FileName != "photo.png" {
			if *item.Codec != "tiff" || item.Animated != nil || item.FrameCount != nil || item.LoopCount != nil || item.DurationMS != nil {
				t.Fatal("TIFF pages fabricated animation", item)
			}
			if item.FileName == "plain.jpg" {
				if item.CaptureAt != nil || item.Orientation != nil {
					t.Fatal("TIFF invented EXIF", item)
				}
			} else if item.Orientation == nil || *item.Orientation != 6 || item.CaptureAt == nil || item.CaptureAt.Format(time.RFC3339Nano) != "2026-11-05T04:34:56.123Z" {
				t.Fatal("TIFF EXIF escaped the UTC-millisecond whitelist", item)
			}
		}
		encoded, _ := json.Marshal(item)
		if bytes.Contains(encoded, []byte("GPS")) || bytes.Contains(encoded, []byte(f.rootPath)) || bytes.Contains(encoded, []byte("camera-secret")) {
			t.Fatal("private TIFF fields escaped publication", string(encoded))
		}
		ids[item.FileName], revisions[item.FileName], snapshots[item.FileName] = item.ID, *item.Revision, string(encoded)
	}
	f.publish(t, "incremental", ".")
	for name, id := range ids {
		item, err := service.Get(t.Context(), f.owner, id)
		encoded, _ := json.Marshal(item)
		if err != nil || string(encoded) != snapshots[name] {
			t.Fatal("unchanged TIFF revised identity/metadata", item, err)
		}
		actual, readErr := os.ReadFile(paths[name])
		info, statErr := os.Stat(paths[name])
		if readErr != nil || statErr != nil || !bytes.Equal(actual, inputs[name]) || info.Mode().Perm() != 0o400 {
			t.Fatal("TIFF publication modified read-only source", name, readErr, statErr)
		}
	}
	for name, replacement := range map[string][]byte{"little_exif.jpg": metadataPNG(t), "photo.png": publicationTIFF(t, "plain")} {
		newPath := writeInput(t, root, "replacement", replacement)
		if err := os.Rename(newPath, paths[name]); err != nil {
			t.Fatal(err)
		}
	}
	f.publish(t, "full", ".")
	for name, codec := range map[string]string{"little_exif.jpg": "png", "photo.png": "tiff"} {
		item, err := service.Get(t.Context(), f.owner, ids[name])
		if err != nil || item.Codec == nil || *item.Codec != codec || item.Orientation != nil || item.CaptureAt != nil || item.DurationMS != nil || item.Revision == nil || *item.Revision == revisions[name] {
			t.Fatal("TIFF magic rewrite retained stale EXIF/revision", item, err)
		}
		if codec == "tiff" && (item.Animated != nil || item.FrameCount != nil || item.LoopCount != nil) {
			t.Fatal("TIFF rewrite retained PNG animation evidence", item)
		}
	}
	for name, revision := range revisions {
		var owner string
		if err := f.db.QueryRow(`SELECT media_id FROM media_revisions WHERE id=?`, revision).Scan(&owner); err != nil || owner != ids[name] {
			t.Fatal("TIFF rewrite lost owned history", owner, err)
		}
	}
}

func TestTIFFPublicationAccountsForUnsupportedAndInvalidInputsWithoutPartialMetadata(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	writeInput(t, root, "Album/good.tif", publicationTIFF(t, "little_exif"))
	unsupported := publicationTIFF(t, "little_exif")
	binary.LittleEndian.PutUint16(unsupported[2:4], 43)
	writeInput(t, root, "Album/unsupported.tif", unsupported)
	invalid := publicationTIFF(t, "little_exif")
	writeInput(t, root, "Album/invalid.tif", invalid[:len(invalid)-1])
	generation := f.publish(t, "full", ".")
	job, err := f.queue.Get(t.Context(), generation.JobID)
	if err != nil || job.Status != "failed" || job.Failed != 2 || job.Total != 9 || job.Done != 9 || job.Succeeded != 7 {
		t.Fatal("TIFF unsupported/corrupt paths became successful metadata", job, err)
	}
	for name, status := range map[string]string{"unsupported.tif": "unsupported", "invalid.tif": "invalid"} {
		var actual string
		var inspection media.Inspection
		var raw string
		if err := f.db.QueryRow(`SELECT status,inspection_json FROM inventory_media WHERE generation_id=? AND relative_path=?`, generation.ID, "Album/"+name).Scan(&actual, &raw); err != nil || json.Unmarshal([]byte(raw), &inspection) != nil || actual != status || inspection.Width != nil || inspection.Codec != nil || inspection.CaptureAt != nil || inspection.Orientation != nil {
			t.Fatal("failed TIFF retained partial trusted staging", actual, inspection, err)
		}
	}
	var missing, versions int
	if err := f.db.QueryRow(`SELECT (SELECT COUNT(*) FROM change_log c WHERE NOT EXISTS(SELECT 1 FROM outbox_events o WHERE o.change_seq=c.seq)),(SELECT COUNT(DISTINCT inspect_version) FROM inventory_media WHERE generation_id=?)`, generation.ID).Scan(&missing, &versions); err != nil || missing != 0 || versions != 1 {
		t.Fatal("TIFF staging/publication lost atomic events or mixed inspectors", missing, versions, err)
	}
}
