//go:build !linux || (!amd64 && !arm64)

package media

import (
	"context"
	"errors"
	"os"
	"testing"

	"gallerygo/internal/jobs"
)

func TestArtifactOrphansUnsupportedHostNeverMutatesDatabaseOrFiles(t *testing.T) {
	cache := t.TempDir()
	svc := New(nil, nil, cache, nil)
	if _, err := svc.reconcileArtifactOrphans(t.Context(), nil, jobs.Lease{}, artifactOrphanRequest{}); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("unsupported immutable cleanup attempted a fallback", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := svc.reconcileArtifactOrphans(ctx, nil, jobs.Lease{}, artifactOrphanRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal("unsupported immutable cleanup ignored cancellation", err)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Fatal("unsupported immutable cleanup touched filesystem", entries, err)
	}
}
