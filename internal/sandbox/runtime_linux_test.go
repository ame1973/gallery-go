//go:build linux && (amd64 || arm64)

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOnlyExactTrustedELFFilesAreGranted(t *testing.T) {
	files, err := runtimeFiles()
	if err != nil || len(files) < 2 || len(files) > 256 {
		t.Fatal("fixed codec dependencies cannot be bounded", len(files), err)
	}
	seen := make(map[string]bool)
	for _, name := range files {
		info, err := os.Stat(name)
		canonical, pathErr := filepath.EvalSymlinks(name)
		if err != nil || pathErr != nil || canonical != name || !info.Mode().IsRegular() || !filepath.IsAbs(name) || seen[name] {
			t.Fatal("dependency grants are not unique canonical regular files", name, err, pathErr)
		}
		seen[name] = true
	}
	for path, expected := range map[string]bool{
		"/usr/lib": true, "/usr/lib/x86_64-linux-gnu": true, "/lib/aarch64-linux-gnu": true,
		"/lib64": true, "/usr/lib64": true, "/usr/lib/a/../b": true,
		"": false, ".": false, "lib": false, "/": false, "/usr/lib-escape": false,
		"/usr/lib/../../media": false, "/usr/lib/$LIB": false, "/tmp/lib": false,
	} {
		if actual := trustedLibraryDirectory(path); actual != expected {
			t.Fatal("unsafe ELF search directory classification", path, actual)
		}
	}
}
