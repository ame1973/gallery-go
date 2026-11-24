package search

import (
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeStageUnicode15Reference(t *testing.T) {
	file, err := os.Open("../../tests/fixtures/unicode15-normalization.txt.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	data, err := io.ReadAll(io.LimitReader(compressed, (4<<20)+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > 4<<20 || fmt.Sprintf("%x", sha256.Sum256(data)) != "fb9ac8cc154a80cad6caac9897af55a4e75176af6f4e2bb6edc2bf8b1d57f326" {
		t.Fatal("unexpected Unicode normalization reference")
	}
	decode := func(field string) string {
		t.Helper()
		var result strings.Builder
		for _, item := range strings.Fields(field) {
			value, err := strconv.ParseUint(item, 16, 32)
			if err != nil || !utf8.ValidRune(rune(value)) {
				t.Fatalf("invalid reference scalar %q", item)
			}
			result.WriteRune(rune(value))
		}
		return result.String()
	}
	rows := 0
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "@") {
			continue
		}
		fields := strings.Split(line, ";")
		if len(fields) != 6 || strings.TrimSpace(fields[5]) != "" {
			t.Fatalf("invalid Unicode reference row %d", rows+1)
		}
		want := decode(fields[3])
		for _, field := range fields[:5] {
			got, err := normalizeStage(decode(field))
			if err != nil || got != want {
				t.Fatalf("Unicode reference row %d differs: %v", rows+1, err)
			}
		}
		rows++
	}
	if rows != 19074 {
		t.Fatalf("Unicode reference has %d rows", rows)
	}
}
