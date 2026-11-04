package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSizeAndFileTypeLimits(t *testing.T) {
	path := writeConfig(t, "#"+strings.Repeat("x", 1<<20))
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("oversized configuration accepted")
	}
	if _, err := LoadConfig(t.TempDir()); err == nil {
		t.Fatal("directory accepted as configuration")
	}
}

func TestConfigRejectsSymlinkCycle(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("b", filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalPath(dir, "a"); err == nil {
		t.Fatal("cyclic storage root accepted")
	}
}
