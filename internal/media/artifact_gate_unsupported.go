//go:build !linux || (!amd64 && !arm64)

package media

import "context"

// Unsupported hosts do not substitute process-local locks or touch the cache.
func (s *Service) lockArtifacts(ctx context.Context, _ bool) (*artifactGate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrVariantUnavailable
}
func (gate *artifactGate) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrVariantUnavailable
}
