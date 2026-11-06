//go:build !linux || (!amd64 && !arm64)

package media

import (
	"context"

	"gallerygo/internal/jobs"
)

// No unsupported host may acquire a process-local substitute or mutate DB/files.
func (s *Service) reconcileArtifactTemps(ctx context.Context, _ *jobs.Queue, _ jobs.Lease, _ artifactTempRequest) (artifactTempReport, error) {
	if err := ctx.Err(); err != nil {
		return artifactTempReport{}, err
	}
	return artifactTempReport{}, ErrVariantUnavailable
}
