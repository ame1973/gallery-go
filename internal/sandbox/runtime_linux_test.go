//go:build linux && (amd64 || arm64)

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestRuntimeFingerprintBindsGrantDescriptorsAndTrustedInstallationEpoch(t *testing.T) {
	name := filepath.Join(t.TempDir(), "owned-runtime-fixture")
	if err := os.WriteFile(name, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := runtimeFingerprint(t.Context(), []string{name}, nil)
	if err != nil || !validRuntimeSignature(pin) {
		t.Fatal("runtime epoch was not pinned", pin, err)
	}
	granted := false
	same, err := runtimeFingerprint(t.Context(), []string{name}, func(path string, fd int) error {
		granted = path == name && fd >= 0
		return nil
	})
	if err != nil || !granted || same != pin {
		t.Fatal("grant and fingerprint used different file authority", same, err)
	}
	replacement := name + "-replacement"
	if err := os.WriteFile(replacement, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, name); err != nil {
		t.Fatal(err)
	}
	changed, err := runtimeFingerprint(t.Context(), []string{name}, nil)
	if err != nil || changed == pin {
		t.Fatal("same-size/mtime trusted runtime replacement retained its epoch", err)
	}
	if _, err := runtimeFingerprint(t.Context(), []string{name}, func(string, int) error { return ErrInput }); !errors.Is(err, ErrInput) {
		t.Fatal("failed grant was hidden", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runtimeFingerprint(ctx, []string{name}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled runtime inspection continued", err)
	}
	if _, err := Signature(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled runtime resolution continued", err)
	}
	for _, files := range [][]string{nil, make([]string, 257), {t.TempDir()}, {name + "-absent"}} {
		if _, err := runtimeFingerprint(t.Context(), files, nil); err == nil {
			t.Fatal("unbounded or invalid runtime file accepted", files)
		}
	}
}

func TestProbeRejectsMissingInvalidAndChangedInstallationPins(t *testing.T) {
	for _, pin := range []string{"", "not-a-policy", strings.Repeat("A", 64), strings.Repeat("0", 63)} {
		if output, err := Probe(t.Context(), probeInput(t), DefaultLimits(), pin); !errors.Is(err, ErrUnavailable) || len(output) != 0 {
			t.Fatal("uncontracted runtime authority accepted", pin, err)
		}
	}
	if output, err := Probe(t.Context(), probeInput(t), DefaultLimits(), strings.Repeat("0", 64)); !errors.Is(err, ErrRuntimeChanged) || len(output) != 0 {
		t.Fatal("different codec installation was used under an old policy", err)
	}
}
