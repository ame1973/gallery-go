//go:build !linux || (!amd64 && !arm64)

package media

import "context"

// Unsupported hosts never substitute an overwriting/non-atomic rename or write
// staging bytes when the immutable no-replace storage boundary is unavailable.
func (s *Service) storeStillArtifact(ctx context.Context, _ string, _ stillArtifact, _ []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrVariantUnavailable
}
