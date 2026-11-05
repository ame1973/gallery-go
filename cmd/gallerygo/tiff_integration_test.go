package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

func scanTIFFFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile("../../tests/fixtures/tiff.json")
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]string
	if json.Unmarshal(raw, &encoded) != nil {
		t.Fatal("invalid owned TIFF vectors")
	}
	result := make(map[string][]byte)
	for name, key := range map[string]string{"capture.tiff": "big_exif", "pages.tif": "multipage", "tiles.tif": "tiled_deflate"} {
		data, err := base64.StdEncoding.DecodeString(encoded[key])
		if err != nil || len(data) == 0 {
			t.Fatal("missing owned TIFF vector", key, err)
		}
		result[name] = data
	}
	return result
}
