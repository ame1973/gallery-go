package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestShellStyleNonceIsFreshAndMatchesPolicy(t *testing.T) {
	assets := fstest.MapFS{"index.html": {Data: []byte(`<html><head><meta name="gallerygo-style-nonce" content="__GALLERYGO_STYLE_NONCE__"></head></html>`)}}
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveAsset(assets, w, r) }))
	previous := ""
	for range 2 {
		r := httptest.NewRequest("GET", "/settings/security", nil)
		r.Header.Set("If-Modified-Since", "Wed, 01 Jan 2099 00:00:00 GMT")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("cache status=%d headers=%v", w.Code, w.Header())
		}
		body := w.Body.String()
		parts := strings.Split(body, `content="`)
		if len(parts) != 2 {
			t.Fatal(body)
		}
		nonce := strings.Split(parts[1], `"`)[0]
		if nonce == "" || nonce == previous || strings.Contains(nonce, "GALLERYGO") {
			t.Fatal("missing or repeated nonce")
		}
		policy := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(policy, "style-src 'self' 'nonce-"+nonce+"'") || strings.Contains(policy, "unsafe-inline") || strings.Contains(policy, "unsafe-eval") {
			t.Fatalf("unsafe/mismatched policy: %s", policy)
		}
		previous = nonce
	}
}
