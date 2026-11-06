//go:build !linux || (!amd64 && !arm64)

package media

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestUnsupportedArtifactStorageNeverWritesOrUsesAnOverwritingFallback(t *testing.T) {
	cache := t.TempDir()
	svc := New(nil, nil, cache, nil)
	if stored, err := svc.storeStillArtifact(t.Context(), "job", stillArtifact{}, nil); !errors.Is(err, ErrVariantUnavailable) || stored != nil {
		t.Fatal("unsupported atomic storage did not fail closed", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if stored, err := svc.storeStillArtifact(ctx, "job", stillArtifact{}, nil); !errors.Is(err, context.Canceled) || stored != nil {
		t.Fatal("unsupported storage ignored cancellation", err)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Fatal("unsupported storage created filesystem state", entries, err)
	}
}
