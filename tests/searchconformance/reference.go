package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"gallerygo/internal/search"

	"golang.org/x/text/unicode/norm"
)

func readCompressedReference(path, digest string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer compressed.Close()
	const maximum = 4 << 20
	data, err := io.ReadAll(io.LimitReader(compressed, maximum+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximum || fmt.Sprintf("%x", sha256.Sum256(data)) != digest {
		return nil, fmt.Errorf("Unicode reference has an unexpected digest or size")
	}
	return data, nil
}

// appendUnicodeReference validates the published Unicode15 corpus before adding
// all five columns to the independently generated search oracle.
func appendUnicodeReference(path string, add func(string)) error {
	data, err := readCompressedReference(path, "fb9ac8cc154a80cad6caac9897af55a4e75176af6f4e2bb6edc2bf8b1d57f326")
	if err != nil {
		return err
	}
	rows := 0
	for number, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "@") {
			continue
		}
		columns, err := parseReferenceLine(line)
		if err != nil {
			return fmt.Errorf("Unicode reference line %d: %w", number+1, err)
		}
		for _, input := range columns {
			if got := norm.NFKC.String(input); got != columns[3] {
				return fmt.Errorf("Unicode reference line %d disagrees with pinned NFKC", number+1)
			}
			add(input)
		}
		rows++
	}
	if rows != 19074 {
		return fmt.Errorf("Unicode normalization reference has %d rows, want 19074", rows)
	}
	return nil
}

// appendLongReference checks independent Python Unicode15 long-segment results.
func appendLongReference(path string, add func(string)) error {
	data, err := readCompressedReference(path, "b04087381843c70ff22239bd71e92645318a39c9d2af76d0e6c89077303c04b5")
	if err != nil {
		return err
	}
	var reference struct {
		Unicode string `json:"unicode"`
		Cases   []struct {
			Name       string `json:"name"`
			Input      string `json:"input"`
			Normalized string `json:"normalized"`
			Error      string `json:"error"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &reference); err != nil {
		return err
	}
	if reference.Unicode != "15.0.0" || len(reference.Cases) != 537 {
		return fmt.Errorf("unexpected Unicode long-reference version or case count")
	}
	for _, sample := range reference.Cases {
		normalized, err := search.Normalize(sample.Input)
		switch sample.Error {
		case "":
			if err != nil || normalized != sample.Normalized {
				return fmt.Errorf("long Unicode reference %q disagrees with the search profile", sample.Name)
			}
		case "limit":
			if !errors.Is(err, search.ErrTextLimit) {
				return fmt.Errorf("long Unicode reference %q did not reach its expected limit", sample.Name)
			}
		default:
			return fmt.Errorf("unexpected long Unicode reference error")
		}
		add(sample.Input)
	}
	return nil
}

func parseReferenceLine(line string) ([5]string, error) {
	var columns [5]string
	fields := strings.Split(line, ";")
	if len(fields) != 6 || strings.TrimSpace(fields[5]) != "" {
		return columns, fmt.Errorf("expected five semicolon-separated columns")
	}
	for index, field := range fields[:5] {
		parts := strings.Fields(field)
		if len(parts) == 0 {
			return columns, fmt.Errorf("empty normalization column")
		}
		scalars := make([]rune, 0, len(parts))
		for _, part := range parts {
			code, err := strconv.ParseUint(part, 16, 32)
			if err != nil || !utf8.ValidRune(rune(code)) {
				return columns, fmt.Errorf("invalid Unicode scalar")
			}
			scalars = append(scalars, rune(code))
		}
		columns[index] = string(scalars)
	}
	return columns, nil
}
