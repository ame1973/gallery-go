//go:build !linux || (!amd64 && !arm64)

package media

import (
	"context"
	"errors"
	"os"
	"testing"

	"gallerygo/internal/jobs"
)

func TestArtifactTempsUnsupportedHostNeverMutatesDatabaseOrFiles(t *testing.T) {
	cache := t.TempDir()
	svc := New(nil, nil, cache, nil)
	request := artifactTempRequest{JobLimit: 1, EntryLimit: 1}
	if _, err := svc.reconcileArtifactTemps(t.Context(), nil, jobs.Lease{}, request); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("unsupported cleanup attempted a fallback", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := svc.reconcileArtifactTemps(ctx, nil, jobs.Lease{}, request); !errors.Is(err, context.Canceled) {
		t.Fatal("unsupported cleanup ignored cancellation", err)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Fatal("unsupported cleanup touched filesystem", entries, err)
	}
}
