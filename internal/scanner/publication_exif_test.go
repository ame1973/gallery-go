package scanner

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gallerygo/internal/media"
)

func publicationJPEGWithEXIF(t *testing.T) ([]byte, []byte) {
	t.Helper()
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 12, 7)), nil); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 95)
	copy(data, "II")
	order := binary.LittleEndian
	order.PutUint16(data[2:4], 42)
	order.PutUint32(data[4:8], 8)
	order.PutUint16(data[8:10], 2)
	entry := func(offset int, tag, kind uint16, count, value uint32) {
		order.PutUint16(data[offset:offset+2], tag)
		order.PutUint16(data[offset+2:offset+4], kind)
		order.PutUint32(data[offset+4:offset+8], count)
		order.PutUint32(data[offset+8:offset+12], value)
	}
	entry(10, 0x0112, 3, 1, 6)
	entry(22, 0x8769, 4, 1, 38)
	order.PutUint16(data[38:40], 2)
	entry(40, 0x9003, 2, 20, 68)
	entry(52, 0x9011, 2, 7, 88)
	copy(data[68:88], "2026:11:05 12:34:56\x00")
	copy(data[88:95], "+08:00\x00")
	payload := append([]byte("Exif\x00\x00"), data...)
	segment := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	result := append([]byte{}, encoded.Bytes()[:2]...)
	result = append(result, segment...)
	result = append(result, payload...)
	result = append(result, encoded.Bytes()[2:]...)
	return result, encoded.Bytes()
}

func TestPublicationCarriesWhitelistedEXIFAndClearsStaleMetadataOnRewrite(t *testing.T) {
	f := inventorySetup(t)
	root := media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}
	input, plain := publicationJPEGWithEXIF(t)
	path := writeInput(t, root, "Album/photo.jpg", input)
	generation := f.publish(t, "full", ".")
	var id, revision string
	var epoch int
	if err := f.db.QueryRow(`SELECT m.id,m.current_revision_id,(SELECT inspect_version FROM inventory_media WHERE generation_id=?) FROM media m`, generation.ID).Scan(&id, &revision, &epoch); err != nil || epoch != inspectVersion {
		t.Fatal("EXIF inspection version not pinned", id, epoch, err)
	}
	service := media.New(f.db, f.auth, t.TempDir(), map[string]media.SourceRoot{"fixture": root})
	value, err := service.Get(t.Context(), f.owner, id)
	want := time.Date(2026, 11, 5, 4, 34, 56, 0, time.UTC)
	if err != nil || value.Orientation == nil || *value.Orientation != 6 || value.CaptureAt == nil || !value.CaptureAt.Equal(want) || value.Width == nil || *value.Width != 12 || value.Height == nil || *value.Height != 7 {
		t.Fatal("published EXIF metadata was not source bound", value, err)
	}
	data, err := json.Marshal(value)
	if err != nil || bytes.Contains(data, []byte(f.rootPath)) || !bytes.Contains(data, []byte(`"capture_at":"2026-11-05T04:34:56Z"`)) {
		t.Fatal(string(data), err)
	}
	f.publish(t, "incremental", ".")
	unchanged, err := service.Get(t.Context(), f.owner, id)
	if err != nil || unchanged.Revision == nil || *unchanged.Revision != revision || unchanged.CaptureAt == nil || !unchanged.CaptureAt.Equal(want) {
		t.Fatal("no-op rescan lost source metadata/revision", unchanged, err)
	}
	if original, err := os.ReadFile(path); err != nil || !bytes.Equal(original, input) {
		t.Fatal("EXIF scan rewrote source", err)
	}
	replacement := writeInput(t, root, "replacement.jpg", plain)
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	f.publish(t, "full", ".")
	changed, err := service.Get(t.Context(), f.owner, id)
	if err != nil || changed.Orientation != nil || changed.CaptureAt != nil || changed.Revision == nil || *changed.Revision == revision {
		t.Fatal("rewritten source retained stale EXIF", changed, err)
	}
	var owner string
	if err := f.db.QueryRow(`SELECT media_id FROM media_revisions WHERE id=?`, revision).Scan(&owner); err != nil || owner != id {
		t.Fatal("EXIF rewrite discarded old owned revision", owner, err)
	}
	if stat, err := os.Stat(filepath.Join(root.Path, "Album", "photo.jpg")); err != nil || stat.Mode().Perm() != 0o400 {
		t.Fatal("source was made writable", stat, err)
	}
}

func TestMetadataInspectorUpgradeCannotResumeAnOlderPin(t *testing.T) {
	f := inventorySetup(t)
	writeInput(t, media.SourceRoot{Path: f.rootPath, Marker: ".mounted"}, "Album/photo.png", metadataPNG(t))
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
	// Versions 1 and 2 predate the GIF frame/duration policy fields. Preserve their
	// original JSON field order when reproducing the historical policy digest.
	oldPolicy := struct {
		MaxPixels    int64
		MaxDimension int
		HeaderBytes  int64
		Timeout      time.Duration
	}{policy.MaxPixels, policy.MaxDimension, policy.HeaderBytes, policy.Timeout}
	for _, older := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
		legacyPolicy := any(oldPolicy)
		if older >= 3 {
			legacyPolicy = policy
		}
		data, _ := json.Marshal(struct {
			Version int
			Policy  any
		}{older, legacyPolicy})
		if older >= 7 {
			pinned, err := policy.PinCodecRuntime(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			data, _ = json.Marshal(struct {
				Version      int
				Policy       media.InspectPolicy
				CodecRuntime string
			}{older, policy, pinned.CodecRuntimePin()})
		}
		digest := sha256.Sum256(data)
		if _, err := f.db.Exec(`UPDATE inventory_generations SET inspect_policy_sha256=? WHERE id=?`, hex.EncodeToString(digest[:]), generation.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`UPDATE inventory_media SET inspect_version=? WHERE generation_id=?`, older, generation.ID); err != nil {
			t.Fatal(err)
		}
		_, err = f.inventory.ProbeMetadata(t.Context(), lease, generation.ID, policy)
		walkError(t, err, ErrGeneration)
		after, err := f.queue.Get(t.Context(), lease.Job.ID)
		if err != nil || after.Counts != before.Counts {
			t.Fatal("inspector upgrade mixed metadata work or duplicated progress", older, before.Counts, after.Counts, err)
		}
		var version, indexed int
		if err := f.db.QueryRow(`SELECT (SELECT inspect_version FROM inventory_media WHERE generation_id=?),(SELECT COUNT(*) FROM media)`, generation.ID).Scan(&version, &indexed); err != nil || version != older || indexed != 0 {
			t.Fatal("old immutable metadata was rewritten or published", version, indexed, err)
		}
	}
}

func TestMetadataRejectsUntrustedOrientationAndNonUTCInstants(t *testing.T) {
	width, height := int64(12), int64(7)
	codec := "jpeg"
	valid := media.Inspection{Kind: "image", MIME: "image/jpeg", Format: "jpeg", Codec: &codec, Width: &width, Height: &height, Status: "ok"}
	for _, orientation := range []int64{0, 9} {
		result := valid
		result.Orientation = &orientation
		if validInspection(result) {
			t.Fatal("out-of-range orientation entered staging")
		}
	}
	result := valid
	capture := time.Date(2026, 11, 5, 12, 34, 56, 0, time.FixedZone("untrusted-local", 8*60*60))
	result.CaptureAt = &capture
	if validInspection(result) {
		t.Fatal("non-UTC capture entered staging")
	}
	capture = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if validInspection(result) {
		t.Fatal("unserializable capture entered staging")
	}
	capture = time.Date(2026, 11, 5, 4, 34, 56, 123456789, time.UTC)
	if validInspection(result) {
		t.Fatal("capture precision beyond DB milliseconds entered staging")
	}
}
