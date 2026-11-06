//go:build !linux || (!amd64 && !arm64)

package media

import "context"

// No weaker filesystem/identity fallback is enabled on unsupported worker hosts.
func openStillInputFile(context.Context, stillInputSnapshot, SourceRoot, int64) (*stillInputFile, error) {
	return nil, errStillInput
}
func verifyStillInputFile(context.Context, *stillInputFile, stillInputSnapshot, SourceRoot, int64) error {
	return errStillInput
}
