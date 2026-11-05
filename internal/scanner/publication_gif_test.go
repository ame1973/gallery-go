package scanner

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"os"
	"testing"

	"gallerygo/internal/media"
)

func TestPublicationPreservesGIFTimingAndClearsItOnFormatRewrite(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 3, 2), palette)
	second.Pix[0] = 1
	var encoded bytes.Buffer
	if err := gif.EncodeAll(&encoded, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{10, 20}, LoopCount: 2}); err != nil {
		t.Fatal(err)
	}
	path := writeInput(t, root, "Album/animation.gif", encoded.Bytes())
	f.publish(t, "full", ".")
	var id, revision string
	var version int64
	if err := f.db.QueryRow(`SELECT id,current_revision_id,version FROM media`).Scan(&id, &revision, &version); err != nil {
		t.Fatal(err)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	value, err := service.Get(t.Context(), f.owner, id)
	if err != nil || value.Animated == nil || !*value.Animated || value.FrameCount == nil || *value.FrameCount != 2 || value.LoopCount == nil || *value.LoopCount != 3 || value.DurationMS == nil || *value.DurationMS != 300 || value.Width == nil || *value.Width != 3 || value.Height == nil || *value.Height != 2 {
		t.Fatal("GIF container evidence was not atomically published", value, err)
	}
	data, err := json.Marshal(value)
	if err != nil || bytes.Contains(data, []byte(f.rootPath)) || !bytes.Contains(data, []byte(`"animated":true`)) || !bytes.Contains(data, []byte(`"loop_count":3`)) {
		t.Fatal("unsafe or incorrect animation DTO", string(data), err)
	}
	items, err := service.AlbumMedia(t.Context(), f.owner, value.AlbumID, "", 30)
	if err != nil || len(items) != 1 || items[0].FrameCount == nil || *items[0].FrameCount != 2 || items[0].DurationMS == nil || *items[0].DurationMS != 300 {
		t.Fatal("detail/list animation evidence diverged", items, err)
	}
	f.publish(t, "incremental", ".")
	var currentRevision string
	var currentVersion int64
	if err := f.db.QueryRow(`SELECT current_revision_id,version FROM media WHERE id=?`, id).Scan(&currentRevision, &currentVersion); err != nil || currentRevision != revision || currentVersion != version {
		t.Fatal("unchanged GIF duplicated revision/version", currentRevision, currentVersion, err)
	}
	if original, err := os.ReadFile(path); err != nil || !bytes.Equal(original, encoded.Bytes()) {
		t.Fatal("GIF inspection rewrote source bytes", err)
	}
	plain := metadataPNG(t)
	replacement := writeInput(t, root, "replacement.png", plain)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	changed, err := service.Get(t.Context(), f.owner, id)
	if err != nil || changed.Revision == nil || *changed.Revision == revision || changed.Animated == nil || *changed.Animated || changed.FrameCount == nil || *changed.FrameCount != 1 || changed.LoopCount == nil || *changed.LoopCount != 1 || changed.DurationMS != nil || changed.Width == nil || *changed.Width != 12 {
		t.Fatal("format rewrite retained stale animation or ignored magic", changed, err)
	}
	var owner string
	if err := f.db.QueryRow(`SELECT media_id FROM media_revisions WHERE id=?`, revision).Scan(&owner); err != nil || owner != id {
		t.Fatal("animation rewrite lost historical revision ownership", owner, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o400 {
		t.Fatal("animation source became writable", info, err)
	}
}

func TestMetadataRejectsInconsistentAnimationEvidence(t *testing.T) {
	width, height, frames, loops, duration, animated := int64(3), int64(2), int64(2), int64(0), int64(300), true
	valid := media.Inspection{Kind: "image", MIME: "image/gif", Format: "gif", Width: &width, Height: &height, FrameCount: &frames, LoopCount: &loops, DurationMS: &duration, Animated: &animated, Status: "ok"}
	if !validInspection(valid) {
		t.Fatal("valid animation rejected")
	}
	for _, field := range []string{"frames", "flag", "loops", "duration", "missing-flag", "missing-frames", "failed"} {
		result := valid
		bad, one := int64(-1), int64(1)
		switch field {
		case "frames":
			result.FrameCount = &bad
		case "flag":
			result.FrameCount = &one
		case "loops":
			result.LoopCount = &bad
		case "duration":
			result.DurationMS = &bad
		case "missing-flag":
			result.Animated = nil
		case "missing-frames":
			result.FrameCount = nil
		case "failed":
			result.Status = "invalid"
		}
		if validInspection(result) {
			t.Fatal("inconsistent animation entered immutable staging", field)
		}
	}
}
