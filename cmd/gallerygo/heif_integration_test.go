package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
)

func scanHEIFFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile("../../tests/fixtures/heif.json")
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]string
	if json.Unmarshal(raw, &encoded) != nil {
		t.Fatal("invalid owned HEIF vectors")
	}
	result := make(map[string][]byte)
	for name, key := range map[string]string{"primary.heic": "collection", "capture.heif": "mirrored", "still.heic": "generic"} {
		data, err := base64.StdEncoding.DecodeString(encoded[key])
		if err != nil || len(data) == 0 {
			t.Fatal("missing owned HEIF vector", key, err)
		}
		result[name] = data
	}
	return result
}
