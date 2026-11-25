//go:build linux && (amd64 || arm64)

package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const artifactMountFixture = `10 1 8:1 / / rw - ext4 /dev/example rw
11 10 8:1 /media/photos /bind-photos ro shared:5 unknown:future - ext4 /dev/example rw
12 10 8:1 /media/photos/Album /bind-album ro - ext4 /dev/example rw
13 10 8:1 /media/photos-old /other ro - ext4 /dev/example rw
14 10 0:42 / /volatile rw - tmpfs tmpfs rw
15 10 8:1 /somewhere /stack rw - ext4 /dev/example rw
16 15 8:1 /media/photos /stack ro - ext4 /dev/example rw
17 10 8:1 /media/space\040name /space\040mount ro - ext4 /dev/example rw
`

func TestArtifactMountInfoAliases(t *testing.T) {
	mounts, err := readArtifactMounts(strings.NewReader(artifactMountFixture))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		leftID      uint64
		left        string
		rightID     uint64
		right       string
		wantOverlap bool
	}{
		{"same", 10, "/media/photos", 11, "/bind-photos", true},
		{"descendant", 10, "/media/photos", 12, "/bind-album/backup", true},
		{"reverse", 12, "/bind-album/backup", 10, "/media/photos", true},
		{"separate", 10, "/data/backups", 11, "/bind-photos", false},
		{"prefix-boundary", 10, "/media/photos", 13, "/other", false},
		{"different-device", 10, "/", 14, "/volatile", false},
		{"stack-lower-id", 15, "/stack/item", 10, "/media/photos", false},
		{"stack-observed-id", 16, "/stack/item", 10, "/media/photos", true},
		{"space-path", 10, "/media/space name", 17, "/space mount/subdir", true},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			overlap, err := artifactMountPathsOverlap(mounts, item.leftID, item.left, item.rightID, item.right)
			if err != nil || overlap != item.wantOverlap {
				t.Fatalf("overlap=%t want=%t error=%v", overlap, item.wantOverlap, err)
			}
		})
	}
	// Pure pathname checks miss this represented bind-mount alias.
	if artifactPathsOverlap("/media/photos", "/bind-album/backup") {
		t.Fatal("counterexample paths unexpectedly overlap lexically")
	}
}

func TestArtifactMountInfoNestedSourceAliases(t *testing.T) {
	fixture := artifactMountFixture + `20 10 0:99 / /source ro - tmpfs tmpfs rw
21 20 8:1 /data /source/nested ro - ext4 source rw
22 20 8:1 /unrelated /source-old/nested ro - ext4 source rw
`
	mounts, err := readArtifactMounts(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	// Comparing only the two roots misses the represented nested alias.
	if overlap, err := artifactMountPathsOverlap(mounts, 10, "/data/backups", 20, "/source"); err != nil || overlap {
		t.Fatalf("unexpected base comparison: overlap=%t error=%v", overlap, err)
	}
	for _, item := range []struct {
		id   uint64
		path string
		want bool
	}{{10, "/data/backups", true}, {10, "/unrelated/backups", false}, {20, "/source/new-backup", true}, {10, "/data-old/backups", false}} {
		overlap, err := artifactMountSourceOverlap(mounts, item.id, item.path, 20, "/source")
		if err != nil || overlap != item.want {
			t.Errorf("path=%s overlap=%t want=%t error=%v", item.path, overlap, item.want, err)
		}
	}
	if _, err := artifactMountSourceOverlap(mounts, 999, "/data", 20, "/source"); err == nil {
		t.Fatal("missing destination mount accepted")
	}
	if _, err := artifactMountSourceOverlap(mounts, 10, "/data", 999, "/source"); err == nil {
		t.Fatal("missing source mount accepted")
	}
	mounts[21] = artifactMount{device: "8:1", root: "/data", point: "/source/nested", filesystem: "different"}
	if _, err := artifactMountSourceOverlap(mounts, 10, "/data", 20, "/source"); err == nil {
		t.Fatal("inconsistent nested mount accepted")
	}
}

func TestArtifactMountInfoLiteralNonseparatorBytes(t *testing.T) {
	for _, suffix := range []string{" ", "\r", "\v", "\f", string([]byte{0xff})} {
		root, point := "/source"+suffix+"name", "/mount"+suffix+"name"
		fixture := fmt.Sprintf("20 10 8:1 %s %s rw - ext4 source rw\n", root, point)
		mounts, err := readArtifactMounts(strings.NewReader(fixture))
		if err != nil {
			t.Errorf("literal nonseparator %q rejected: %v", suffix, err)
			continue
		}
		if mounts[20].root != root || mounts[20].point != point {
			t.Errorf("literal nonseparator %q changed", suffix)
		}
	}
}

func TestArtifactMountInfoOpaqueSuperOptions(t *testing.T) {
	fixture := `30 10 0:70 / /windows rw - 9p C:\134Test\040Directory rw,aname=drvfs;path=C:\Test Directory;symlinkroot=/mnt/,cache=5`
	mounts, err := readArtifactMounts(strings.NewReader(fixture + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	mount := mounts[30]
	if mount.point != "/windows" || mount.root != "/" || mount.device != "0:70" || mount.filesystem != "9p" {
		t.Fatalf("opaque options changed parsed identity: %+v", mount)
	}
}

func TestArtifactMountInfoRejectsMalformed(t *testing.T) {
	valid := "10 1 8:1 / / rw - ext4 source rw\n"
	inputs := []string{
		"", "\n", "10 1", valid + valid,
		strings.Replace(valid, "- ext4", "- ", 1),
		strings.Replace(valid, "ext4 source", "ext4 ", 1),
		strings.Replace(valid, " rw -", "  -", 1),
		strings.Replace(valid, "source rw", "source ", 1),
		strings.Replace(valid, "10 1", "0 1", 1),
		strings.Replace(valid, "10 1", "010 1", 1),
		strings.Replace(valid, "10 1", "10 -1", 1),
		strings.Replace(valid, "8:1", "8", 1),
		strings.Replace(valid, "8:1", "8:4294967296", 1),
		strings.Replace(valid, "8:1", "8:01", 1),
		strings.Replace(valid, "8:1", "+8:1", 1),
		strings.Replace(valid, "/ /", "/../source /", 1),
		strings.Replace(valid, "/ /", "/ //", 1),
		strings.Replace(valid, "/ /", "relative /", 1),
		strings.Replace(valid, "/ /", "/bad\x00 /", 1),
		strings.Replace(valid, "/ /", `/bad\777 /`, 1),
		strings.Replace(valid, "/ /", `/bad\04 /`, 1),
		strings.Replace(valid, " - ", " optional ", 1),
		valid + "\n", strings.TrimSuffix(valid, " rw\n") + "\n",
		strings.Repeat("a", maxArtifactMountInfoBytes+1),
		"10 1 8:1 / / rw " + strings.Repeat("x", 64<<10) + " - ext4 source rw\n",
	}
	for index, input := range inputs {
		if _, err := readArtifactMounts(strings.NewReader(input)); err == nil {
			t.Errorf("malformed mount input %d accepted", index)
		}
	}
	var records strings.Builder
	for id := 1; id <= 16385; id++ {
		fmt.Fprintf(&records, "%d 1 8:1 / / rw - ext4 source rw\n", id)
	}
	if _, err := readArtifactMounts(strings.NewReader(records.String())); err == nil {
		t.Fatal("excessive record count accepted")
	}
	failure := errors.New("test reader error")
	if _, err := readArtifactMounts(artifactMountErrorReader{failure}); !errors.Is(err, failure) {
		t.Fatalf("reader error lost: %v", err)
	}
}

type artifactMountErrorReader struct{ err error }

func (r artifactMountErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestArtifactMountPathEscapesAndIdentity(t *testing.T) {
	path, err := decodeArtifactMountPath(`/space\040tab\011newline\012slash\134`)
	if err != nil || path != "/space tab\tnewline\nslash\\" {
		t.Fatalf("decoded path=%q error=%v", path, err)
	}
	mounts, err := readArtifactMounts(strings.NewReader(artifactMountFixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		id   uint64
		path string
	}{{999, "/"}, {11, "/unrelated"}, {11, "/bind-photos-old"}, {11, "/bind-photos/../outside"}, {11, "relative"}, {11, "/bind-photos/\x00"}} {
		if _, _, err := artifactMountLocation(mounts, item.id, item.path); err == nil {
			t.Errorf("invalid observed identity accepted: %+v", item)
		}
	}
	if _, err := artifactMountPathsOverlap(mounts, 999, "/", 10, "/"); err == nil {
		t.Fatal("invalid left identity accepted")
	}
	if _, err := artifactMountPathsOverlap(mounts, 10, "/", 999, "/"); err == nil {
		t.Fatal("invalid right identity accepted")
	}
	mounts[18] = artifactMount{device: "8:1", root: "/", point: "/", filesystem: "other"}
	if _, err := artifactMountPathsOverlap(mounts, 10, "/", 18, "/"); err == nil {
		t.Fatal("inconsistent device/filesystem identity accepted")
	}
}

func TestArtifactMountInfoTemporaryDirectory(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	var stat unix.Statx_t
	if err := unix.Statx(int(directory.Fd()), "", unix.AT_EMPTY_PATH, unix.STATX_BASIC_STATS|unix.STATX_MNT_ID, &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 {
		t.Fatal("kernel did not return mount identity")
	}
	info, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer info.Close()
	mounts, err := readArtifactMounts(info)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(directory.Name())
	if err != nil {
		t.Fatal(err)
	}
	mount, _, err := artifactMountLocation(mounts, stat.Mnt_id, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d:%d", stat.Dev_major, stat.Dev_minor); mount.device != want {
		t.Fatalf("device identity mismatch: got=%s want=%s", mount.device, want)
	}
	// This observes only the freshly created temporary directory. It does not
	// create a bind mount, open configured sources or prove destination isolation.
}
