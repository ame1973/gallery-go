package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gallerygo/internal/auth"
)

func TestBootstrapCommandAndSecretInput(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte("sources: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(t.Context(), []string{"migrate", "--config", config}, &out); err != nil {
		t.Fatal(err)
	}
	args := []string{"bootstrap-admin", "--config", config, "--username", "owner", "--password-stdin"}
	password := "a secure bootstrap passphrase"
	if err := runWithInput(t.Context(), args, strings.NewReader(password+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Administrator owner created") || strings.Contains(out.String(), password) {
		t.Fatalf("unsafe output: %s", out.String())
	}
	if err := runWithInput(t.Context(), args, strings.NewReader(password), &out); !errors.Is(err, auth.ErrBootstrapped) {
		t.Fatalf("repeat bootstrap: %v", err)
	}
	for _, bad := range []string{strings.Repeat("x", 73), "first line\nsecond line"} {
		if _, err := readPassword(strings.NewReader(bad), &out, true); err == nil {
			t.Fatal("bad stdin password accepted")
		}
	}
	if _, err := readPassword(strings.NewReader(password), &out, false); err == nil {
		t.Fatal("noninteractive input silently accepted")
	}
	got, err := readPassword(strings.NewReader("  keep leading and trailing spaces  \r\n"), &out, true)
	if err != nil || got != "  keep leading and trailing spaces  " {
		t.Fatalf("password whitespace changed: %q %v", got, err)
	}
}
