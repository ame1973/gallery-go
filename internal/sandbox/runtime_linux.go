//go:build linux && (amd64 || arm64)

package sandbox

import (
	"debug/elf"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// runtimeFiles resolves only the fixed trusted tool's ELF dependencies. File rules,
// not broad /usr/lib directory grants, prevent source mounts beneath a system
// directory from accidentally becoming readable by a malicious media file.
func runtimeFiles() ([]string, error) {
	queue := []string{probeBinary, dynamicLoader}
	seen := make(map[string]bool)
	result := make([]string, 0, 128)
	for len(queue) > 0 {
		name, err := filepath.EvalSymlinks(queue[0])
		queue = queue[1:]
		if err != nil {
			return nil, err
		}
		if seen[name] {
			continue
		}
		if len(result) >= 256 {
			return nil, ErrUnavailable
		}
		info, err := os.Stat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 512<<20 {
			return nil, ErrUnavailable
		}
		file, err := elf.Open(name)
		if err != nil {
			return nil, err
		}
		libraries, err := file.ImportedLibraries()
		paths := append([]string{}, libraryDirectories...)
		for _, tag := range []elf.DynTag{elf.DT_RUNPATH, elf.DT_RPATH} {
			entries, pathErr := file.DynString(tag)
			if pathErr != nil {
				file.Close()
				return nil, pathErr
			}
			for _, entry := range entries {
				for _, directory := range strings.Split(entry, ":") {
					directory = strings.ReplaceAll(directory, "${ORIGIN}", filepath.Dir(name))
					directory = strings.ReplaceAll(directory, "$ORIGIN", filepath.Dir(name))
					if trustedLibraryDirectory(directory) {
						paths = append(paths, directory)
					}
				}
			}
		}
		file.Close()
		if err != nil {
			return nil, err
		}
		seen[name] = true
		result = append(result, name)
		for _, library := range libraries {
			if library == "" || len(library) > 255 || filepath.Base(library) != library || strings.ContainsAny(library, "/\\\x00") {
				return nil, ErrUnavailable
			}
			found := false
			for _, directory := range paths {
				path := filepath.Join(directory, library)
				if _, err := os.Stat(path); err == nil {
					queue = append(queue, path)
					found = true
					break
				} else if !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
			}
			if !found {
				return nil, ErrUnavailable
			}
		}
	}
	return result, nil
}

func trustedLibraryDirectory(path string) bool {
	if !filepath.IsAbs(path) || strings.Contains(path, "$") {
		return false
	}
	path = filepath.Clean(path)
	for _, root := range []string{"/usr/lib", "/usr/lib64", "/lib", "/lib64"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}
