//go:build linux && (amd64 || arm64)

package media

import (
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

const maxArtifactMountInfoBytes = 4 << 20

type artifactMount struct {
	device     string
	root       string
	point      string
	filesystem string
}

// readArtifactMounts decodes one bounded mount-namespace description. Mount IDs,
// rather than path-prefix order, select mounts because mounts may be stacked.
// This does not establish source isolation across hidden backing filesystems,
// mount changes, network exports or same-UID directory mutation.
func readArtifactMounts(input io.Reader) (map[uint64]artifactMount, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxArtifactMountInfoBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxArtifactMountInfoBytes {
		return nil, errors.New("invalid mount description size")
	}
	mounts := make(map[uint64]artifactMount)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if len(line) > 64<<10 || len(mounts) >= 16384 {
			return nil, errors.New("mount description limit exceeded")
		}
		// Only ASCII space separates fields; other literal filename bytes must
		// survive unchanged rather than being treated as Unicode whitespace.
		fields := strings.Split(line, " ")
		separator := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				separator = i
				break
			}
		}
		// Filesystem-specific super options are an opaque remainder: for example,
		// WSL 9p may put unescaped spaces inside its aname option. No access or
		// mount-safety decision may be inferred from these unparsed options.
		if separator < 6 || len(fields) < separator+4 {
			return nil, errors.New("invalid mount description record")
		}
		if fields[5] == "" || fields[separator+1] == "" || fields[separator+2] == "" || fields[separator+3] == "" {
			return nil, errors.New("missing mount description field")
		}
		id, idErr := artifactMountNumber(fields[0], 64)
		_, parentErr := artifactMountNumber(fields[1], 64)
		device := strings.Split(fields[2], ":")
		if idErr != nil || parentErr != nil || id == 0 || len(device) != 2 {
			return nil, errors.New("invalid mount identity")
		}
		if _, err := artifactMountNumber(device[0], 32); err != nil {
			return nil, errors.New("invalid mount device")
		}
		if _, err := artifactMountNumber(device[1], 32); err != nil {
			return nil, errors.New("invalid mount device")
		}
		if _, duplicate := mounts[id]; duplicate {
			return nil, errors.New("duplicate mount identity")
		}
		root, rootErr := decodeArtifactMountPath(fields[3])
		point, pointErr := decodeArtifactMountPath(fields[4])
		if rootErr != nil || pointErr != nil {
			return nil, errors.New("invalid mount path")
		}
		mounts[id] = artifactMount{device: fields[2], root: root, point: point, filesystem: fields[separator+1]}
	}
	return mounts, nil
}

func artifactMountNumber(value string, bits int) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, bits)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return 0, errors.New("invalid mount number")
	}
	return n, nil
}

func decodeArtifactMountPath(value string) (string, error) {
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			result.WriteByte(value[i])
			continue
		}
		if i+3 >= len(value) {
			return "", errors.New("incomplete mount path escape")
		}
		var decoded byte
		switch value[i+1 : i+4] {
		case "040":
			decoded = ' '
		case "011":
			decoded = '\t'
		case "012":
			decoded = '\n'
		case "134":
			decoded = '\\'
		default:
			return "", errors.New("unsupported mount path escape")
		}
		result.WriteByte(decoded)
		i += 3
	}
	path := result.String()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return "", errors.New("mount path is not canonical")
	}
	return path, nil
}

// artifactMountLocation translates an existing path and its observed mount ID into
// a filesystem-relative path. A caller must obtain and pin the actual mount ID;
// this helper must not be used with an ID guessed from a pathname prefix.
func artifactMountLocation(mounts map[uint64]artifactMount, id uint64, path string) (artifactMount, string, error) {
	mount, found := mounts[id]
	if !found || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return artifactMount{}, "", errors.New("mount path identity is unavailable")
	}
	relative, err := filepath.Rel(mount.point, path)
	if err != nil || (relative != "." && !filepath.IsLocal(relative)) {
		return artifactMount{}, "", errors.New("path is outside the observed mount")
	}
	return mount, filepath.Join(mount.root, relative), nil
}

// artifactMountSourceOverlap also considers mounts beneath the source's namespace
// path. Hidden/stacked records are deliberately included, so an uncertain topology
// can reject a safe destination rather than overlooking a represented source alias.
// False still does not establish safety against unrepresented backing aliases.
func artifactMountSourceOverlap(mounts map[uint64]artifactMount, destinationID uint64, destination string, sourceID uint64, source string) (bool, error) {
	dest, destPath, err := artifactMountLocation(mounts, destinationID, destination)
	if err != nil {
		return false, err
	}
	baseOverlap, err := artifactMountPathsOverlap(mounts, destinationID, destination, sourceID, source)
	if err != nil {
		return false, err
	}
	if baseOverlap || artifactPathsOverlap(destination, source) {
		return true, nil
	}
	for _, mount := range mounts {
		relative, err := filepath.Rel(source, mount.point)
		if err != nil || (relative != "." && !filepath.IsLocal(relative)) || mount.device != dest.device {
			continue
		}
		if mount.filesystem != dest.filesystem {
			return false, errors.New("inconsistent nested filesystem identity")
		}
		if artifactPathsOverlap(destPath, mount.root) {
			return true, nil
		}
	}
	return false, nil
}

// artifactMountPathsOverlap detects overlap represented by two observed mount IDs.
// A false result is NOT permission to write: nested mounts, stacked/backing
// filesystem aliases, source availability and topology stability need separate
// checks in the future caller. No comparison alone grants authority to write.
func artifactMountPathsOverlap(mounts map[uint64]artifactMount, leftID uint64, left string, rightID uint64, right string) (bool, error) {
	a, aPath, err := artifactMountLocation(mounts, leftID, left)
	if err != nil {
		return false, err
	}
	b, bPath, err := artifactMountLocation(mounts, rightID, right)
	if err != nil {
		return false, err
	}
	if a.device != b.device {
		return false, nil
	}
	if a.filesystem != b.filesystem {
		return false, errors.New("inconsistent filesystem identity")
	}
	return artifactPathsOverlap(aPath, bPath), nil
}
