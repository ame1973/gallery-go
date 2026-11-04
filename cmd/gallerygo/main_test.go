package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommands(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte("sources: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"doctor", "--config", config}, &out); err == nil {
		t.Fatal("doctor accepted unmigrated database")
	}
	for range 2 {
		if err := run(t.Context(), []string{"migrate", "--config", config}, &out); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	if err := run(t.Context(), []string{"doctor", "--config", config}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"database":"ok"`) {
		t.Fatalf("doctor output: %s", out.String())
	}
	out.Reset()
	if err := run(t.Context(), []string{"version"}, &out); err != nil || strings.TrimSpace(out.String()) != version {
		t.Fatalf("version: %s %v", out.String(), err)
	}
	for _, args := range [][]string{nil, {"unknown"}, {"version", "extra"}, {"migrate", "--unknown"}, {"migrate", "extra"}, {"migrate", "--config", filepath.Join(dir, "missing")}} {
		if err := run(t.Context(), args, &out); err == nil {
			t.Fatalf("accepted invalid args: %v", args)
		}
	}
}
