//go:build !linux || (!amd64 && !arm64)

package media

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPreparedStillInputUnsupportedHostNeverCreatesSourceOrFallback(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "must-remain-missing")
	if _, err := openStillInputFile(t.Context(), stillInputSnapshot{}, SourceRoot{Path: missing, Marker: ".marker"}, 1<<20); !errors.Is(err, errStillInput) {
		t.Fatal("unsupported host accepted a source fallback", err)
	}
	if err := verifyStillInputFile(t.Context(), nil, stillInputSnapshot{}, SourceRoot{Path: missing, Marker: ".marker"}, 1<<20); !errors.Is(err, errStillInput) {
		t.Fatal("unsupported source verification did not fail closed", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsupported helper touched missing source", err)
	}
}
