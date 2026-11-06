//go:build !linux || (!amd64 && !arm64)

package media

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestUnsupportedArtifactGateNeverCreatesLockOrLocalFallback(t *testing.T) {
	cache := t.TempDir()
	svc := New(nil, nil, cache, nil)
	for _, exclusive := range []bool{false, true} {
		if gate, err := svc.lockArtifacts(t.Context(), exclusive); !errors.Is(err, ErrVariantUnavailable) || gate != nil {
			t.Fatal("unsupported gate did not fail closed", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if gate, err := svc.lockArtifacts(ctx, true); !errors.Is(err, context.Canceled) || gate != nil {
		t.Fatal("unsupported gate ignored cancellation", err)
	}
	if err := (*artifactGate)(nil).check(t.Context()); !errors.Is(err, ErrVariantUnavailable) {
		t.Fatal("unsupported gate became publication evidence", err)
	}
	if entries, err := os.ReadDir(cache); err != nil || len(entries) != 0 {
		t.Fatal("unsupported gate touched filesystem state", entries, err)
	}
}
