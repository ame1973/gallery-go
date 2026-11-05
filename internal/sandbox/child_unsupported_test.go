//go:build !linux || (!amd64 && !arm64)

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnsupportedPlatformsNeverLaunchAnUnrestrictedChild(t *testing.T) {
	name := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(name, []byte("test-owned media input"), 0o400); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if output, err := Probe(t.Context(), input, DefaultLimits(), strings.Repeat("0", 64)); !errors.Is(err, ErrUnavailable) || len(output) != 0 {
		t.Fatal("unsupported platform did not fail closed", err)
	}
	if code := RunImageChild(nil); code != 70 {
		t.Fatal("unsupported image child did not reject execution", code)
	}
	if output, err := ImageHeader(t.Context(), input, DefaultLimits(), strings.Repeat("0", 64)); !errors.Is(err, ErrUnavailable) || len(output) != 0 {
		t.Fatal("unsupported image platform failed open", err)
	}
	if _, err := ImageSignature(t.Context()); !errors.Is(err, ErrUnavailable) {
		t.Fatal("unsupported image signature claimed capability", err)
	}
	if code := RunChild(nil); code != 70 {
		t.Fatal("unsupported child did not reject execution", code)
	}
}
