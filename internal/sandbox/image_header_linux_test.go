//go:build linux && (amd64 || arm64)

package sandbox

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func imageSignature(t *testing.T) string {
	t.Helper()
	pin, err := ImageSignature(t.Context())
	if err != nil {
		t.Fatal("fixed image runtime cannot be pinned", err)
	}
	return pin
}

func TestActualImageHeaderProfileReturnsOnlyTheFixedGeometrySummary(t *testing.T) {
	input := probeInput(t)
	pin := imageSignature(t)
	output, err := ImageHeader(t.Context(), input, DefaultLimits(), pin)
	if err != nil {
		binary, _ := os.Executable()
		command := exec.CommandContext(t.Context(), binary, ImageHeaderCommand, "536870912", "10", "34359738368", pin)
		command.ExtraFiles, command.Env = []*os.File{input}, childEnvironment
		diagnostic, failure := command.CombinedOutput()
		t.Fatalf("fixed image isolation failed: %v child=%v\n%s", err, failure, diagnostic)
	}
	if !bytes.HasPrefix(output, []byte("/proc/self/fd/3: 12x7 ")) || bytes.Count(output, []byte("\n")) != 1 || bytes.Contains(output, []byte(input.Name())) || bytes.Contains(output, []byte("width:")) {
		t.Fatal("unexpected or unprojected image output", string(output))
	}
	if output, err := ImageHeader(t.Context(), input, DefaultLimits(), strings.Repeat("0", 64)); !errors.Is(err, ErrRuntimeChanged) || len(output) != 0 {
		t.Fatal("image runtime epoch was not fenced", err)
	}
}

func TestActualImageHeaderRecognizesTheOwnedGenericAVIFProfile(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/avif.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors map[string]string
	if json.Unmarshal(raw, &vectors) != nil {
		t.Fatal("invalid owned AVIF vectors")
	}
	data, err := base64.StdEncoding.DecodeString(vectors["still"])
	if err != nil {
		t.Fatal(err)
	}
	copy(data[8:12], "mif1")
	name := filepath.Join(t.TempDir(), "read-only")
	if err := os.WriteFile(name, data, 0o400); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := ImageHeader(t.Context(), input, DefaultLimits(), imageSignature(t))
	if err != nil {
		binary, _ := os.Executable()
		command := exec.CommandContext(t.Context(), binary, ImageHeaderCommand, "536870912", "10", "34359738368", imageSignature(t))
		command.ExtraFiles, command.Env = []*os.File{input}, childEnvironment
		diagnostic, failure := command.CombinedOutput()
		t.Fatalf("owned generic AVIF isolation failed: %v child=%v\n%s", err, failure, diagnostic)
	}
	if !bytes.HasPrefix(output, []byte("/proc/self/fd/3: 16x12 ")) || !bytes.Contains(output, []byte("heifload")) {
		t.Fatal("owned generic AVIF capability missing", string(output))
	}
	if actual, err := os.ReadFile(name); err != nil || !bytes.Equal(actual, data) {
		t.Fatal("image probe changed source", err)
	}
}
