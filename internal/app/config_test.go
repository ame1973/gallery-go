package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gallerygo/internal/auth"
)

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigDefaultsAndRelativePaths(t *testing.T) {
	p := writeConfig(t, "sources: []\n")
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Database != filepath.Join(filepath.Dir(p), "data", "gallerygo.db") || c.BaseURL != "https://localhost:8443" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if _, err := os.Stat(filepath.Dir(c.Database)); !os.IsNotExist(err) {
		t.Fatal("loading config wrote storage directories")
	}
}

func TestRejectUnsafeOrAmbiguousConfig(t *testing.T) {
	cases := []string{
		"unknown: true", "listen: localhost:0", "listen: invalid", "listen: example.com:8080", "listen: :65536",
		"base_url: http://localhost", "base_url: https://user:pass@example.com", "base_url: https://example.com/path", "base_url: https://example.com?x=1", "base_url: https://example.com#hash", "base_url: https://example.com:65536",
		"database: ''", "database: https://example.com/db", "cache_root: data/cache", "web_root: .",
		"sources:\n  - {name: Family, root: cache/photos, marker: .mounted}",
		"sources:\n  - {name: Family, root: photos, marker: ../outside}",
		"sources:\n  - {name: Family, root: photos, marker: .}",
		"sources:\n  - {name: Family, root: photos, marker: .mounted}\n  - {name: Family, root: other, marker: .mounted}",
		"sources:\n  - {name: A, root: photos, marker: .mounted}\n  - {name: B, root: photos/child, marker: .mounted}",
		"sources: []\n---\nsources: []", "sources: []\n---\n", "listen: :8080\nlisten: :8081",
	}
	for _, text := range cases {
		t.Run(strings.ReplaceAll(text, "\n", " "), func(t *testing.T) {
			if _, err := LoadConfig(writeConfig(t, text)); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestConfigSymlinkOverlap(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "photos"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "photos"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	c := Config{Listen: ":8080", BaseURL: "https://localhost", Database: "data/db", CacheRoot: "alias/cache", WebRoot: "web", Sources: []Source{{Name: "A", Root: "photos", Marker: ".mounted"}}}
	c.Auth = auth.DefaultPolicy()
	if err := c.validate(dir); err == nil {
		t.Fatal("symlink overlap accepted")
	}
}

func TestConfigMissingAndEmpty(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing explicit config accepted")
	}
	if _, err := LoadConfig(writeConfig(t, "")); err == nil {
		t.Fatal("empty config accepted")
	}
}
