//go:build !linux || (!amd64 && !arm64)

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
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
	if output, err := Probe(t.Context(), input, DefaultLimits()); !errors.Is(err, ErrUnavailable) || len(output) != 0 {
		t.Fatal("unsupported platform did not fail closed", err)
	}
	if code := RunChild(nil); code != 70 {
		t.Fatal("unsupported child did not reject execution", code)
	}
}
