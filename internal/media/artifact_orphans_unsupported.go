//go:build !linux || (!amd64 && !arm64)

package media

import (
	"context"

	"gallerygo/internal/jobs"
)

func (s *Service) reconcileArtifactOrphans(ctx context.Context, _ *jobs.Queue, _ jobs.Lease, _ artifactOrphanRequest) (artifactOrphanReport, error) {
	if err := ctx.Err(); err != nil {
		return artifactOrphanReport{}, err
	}
	return artifactOrphanReport{}, ErrVariantUnavailable
}
