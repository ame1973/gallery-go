//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gallerygo/internal/sandbox"
)

func TestCompiledPrivateCodecChildNeedsNoConfigAndNeverAcceptsAPath(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "gallerygo")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewNRGBA(image.Rect(0, 0, 12, 7))); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "read-only.png")
	if err := os.WriteFile(name, imageBytes.Bytes(), 0o400); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	command := exec.CommandContext(t.Context(), binary, sandbox.ChildCommand, "536870912", "10", "34359738368")
	command.ExtraFiles = []*os.File{input}
	command.Dir = t.TempDir() // No config, web asset, database or source-root registry.
	output, err := command.Output()
	var value struct {
		Streams []struct {
			Width, Height int
		} `json:"streams"`
	}
	if err != nil || json.Unmarshal(output, &value) != nil || len(value.Streams) != 1 || value.Streams[0].Width != 12 || value.Streams[0].Height != 7 || bytes.Contains(output, []byte(name)) {
		t.Fatal("compiled sandbox child failed or leaked a source path", string(output), err)
	}
	for _, arguments := range [][]string{
		{sandbox.ChildCommand},
		{sandbox.ChildCommand, name},
		{sandbox.ChildCommand, "536870912", "10", "34359738368"}, // Missing input descriptor.
		{sandbox.ChildCommand, "1", "10", "34359738368"},
		{sandbox.ChildCommand, "536870912", "0", "34359738368"},
		{sandbox.ChildCommand, "536870912", "10", "-1"},
		{sandbox.ChildCommand, "18446744073709551616", "10", "34359738368"},
		{sandbox.ChildCommand, "536870912", "10", "34359738368", name},
	} {
		output, err := exec.CommandContext(t.Context(), binary, arguments...).CombinedOutput()
		if err == nil || bytes.Contains(output, []byte(name)) {
			t.Fatal("private child accepted a path/invalid authority or leaked it", arguments, string(output), err)
		}
	}
	if original, err := os.ReadFile(name); err != nil || !bytes.Equal(original, imageBytes.Bytes()) {
		t.Fatal("sandbox child wrote the source", err)
	}
}
