package main

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseReferenceLine(t *testing.T) {
	columns, err := parseReferenceLine("1E0A 0323;1E0C 0307;0044 0323 0307;1E0C 0307;0044 0323 0307;")
	if err != nil || columns != [5]string{"Ḍ̇", "Ḍ̇", "Ḍ̇", "Ḍ̇", "Ḍ̇"} {
		t.Fatal("valid reference row was not decoded", columns, err)
	}
	for _, line := range []string{
		"",
		"0041;0041;0041;0041;0041",
		"0041;0041;;0041;0041;",
		"0041;0041;D800;0041;0041;",
		"0041;0041;110000;0041;0041;",
		"0041;0041;not-hex;0041;0041;",
	} {
		if _, err := parseReferenceLine(line); err == nil {
			t.Fatal("malformed reference row was accepted", line)
		}
	}
}

func TestUnicodeReferenceConformanceAndDigest(t *testing.T) {
	count := 0
	if err := appendUnicodeReference("../fixtures/unicode15-normalization.txt.gz", func(string) { count++ }); err != nil {
		t.Fatal(err)
	}
	if count != 19074*5 {
		t.Fatal("reference did not contain every normalization column", count)
	}
	path := filepath.Join(t.TempDir(), "altered.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	if _, err := compressed.Write([]byte(strings.Repeat("x", 4<<20+1))); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := appendUnicodeReference(path, func(string) {}); err == nil {
		t.Fatal("oversized or altered reference was accepted")
	}
	if err := appendLongReference(path, func(string) {}); err == nil {
		t.Fatal("oversized or altered long reference was accepted")
	}
}

func TestLongUnicodeReferenceConformanceAndDigest(t *testing.T) {
	count := 0
	if err := appendLongReference("../fixtures/search-long-unicode15.json.gz", func(string) { count++ }); err != nil {
		t.Fatal(err)
	}
	if count != 537 {
		t.Fatalf("unexpected long reference case count: %d", count)
	}
}
