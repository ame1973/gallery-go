package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
)

type scanVideoFixture struct {
	data                 []byte
	codec, mime, capture string
	orientation          int64
}

func scanVideoFixtures(t *testing.T) map[string]scanVideoFixture {
	t.Helper()
	raw, err := os.ReadFile("../../tests/fixtures/video.json")
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]string
	if json.Unmarshal(raw, &encoded) != nil {
		t.Fatal("invalid owned video vectors")
	}
	fixtures := make(map[string]scanVideoFixture)
	for _, item := range []struct {
		name, vector, codec, mime, capture string
		orientation                        int64
	}{
		{"clip.mp4", "mp4", "h264", "video/mp4", "2026-11-05T04:34:56Z", 6},
		{"clip.mov", "mov", "h264", "video/quicktime", "2026-11-05T04:34:56.123Z", 6},
		{"clip.mkv", "matroska", "ffv1", "video/x-matroska", "2026-11-05T04:34:56.123Z", 0},
		{"clip.webm", "webm", "vp8", "video/webm", "2026-11-05T04:34:56.123Z", 0},
	} {
		data, err := base64.StdEncoding.DecodeString(encoded[item.vector])
		if err != nil || len(data) == 0 {
			t.Fatal("missing owned video vector", item.vector, err)
		}
		fixtures[item.name] = scanVideoFixture{data, item.codec, item.mime, item.capture, item.orientation}
	}
	return fixtures
}

func scanAVIFFixtures(t *testing.T) map[string][]byte {
	t.Helper()
	raw, err := os.ReadFile("../../tests/fixtures/avif.json")
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]string
	if json.Unmarshal(raw, &encoded) != nil {
		t.Fatal("invalid owned AVIF vectors")
	}
	result := make(map[string][]byte)
	for name, key := range map[string]string{"still.avif": "mirrored", "motion.avif": "animated"} {
		data, err := base64.StdEncoding.DecodeString(encoded[key])
		if err != nil || len(data) == 0 {
			t.Fatal("missing owned AVIF vector", key, err)
		}
		result[name] = data
	}
	return result
}

func assertCompiledOriginal(t *testing.T, api *scanHTTPSClient, id string, source []byte, mime string) {
	t.Helper()
	var original []byte
	headers := api.request("GET", "/media/"+id+"/original", nil, 200, &original)
	if !bytes.Equal(original, source) || headers.Get("Content-Type") != mime || headers.Get("Cache-Control") != "private, no-store" {
		t.Fatal("compiled video probe rewrote bytes or bypassed protected serving", headers)
	}
	request, err := http.NewRequestWithContext(t.Context(), "GET", api.origin+"/api/v2/media/"+id+"/original", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=4-11")
	response, err := api.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 64))
	response.Body.Close()
	if readErr != nil || response.StatusCode != 206 || !bytes.Equal(data, source[4:12]) || response.Header.Get("Content-Range") != fmt.Sprintf("bytes 4-11/%d", len(source)) || response.Header.Get("ETag") != headers.Get("ETag") {
		t.Fatal("actual media lost byte/revision boundaries", response.StatusCode, readErr)
	}
	anonymous := &http.Client{Transport: api.client.Transport, Timeout: api.client.Timeout}
	unauthenticated := request.Clone(t.Context())
	unauthenticated.Header.Del("Cookie")
	response, err = anonymous.Do(unauthenticated)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 || response.Header.Get("ETag") != "" || response.Header.Get("Content-Range") != "" {
		t.Fatal("anonymous media disclosed private content metadata", response.StatusCode)
	}
}
