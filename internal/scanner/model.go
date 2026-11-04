// Package scanner builds complete, bounded, root-confined inventories before any
// indexed media reconciliation. Source files are never opened for write.
package scanner

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrPolicy      = errors.New("invalid scan resource limits")
	ErrLimit       = errors.New("scan resource limit exceeded")
	ErrChanged     = errors.New("source changed while inventory was being read")
	ErrCycle       = errors.New("directory symlink cycle detected")
	ErrUnsafeEntry = errors.New("source contains an unsafe or unsupported filesystem entry")
)

type Policy struct {
	MaxDepth     int           `yaml:"max_depth"`
	MaxFiles     int64         `yaml:"max_files"`
	MaxFileBytes int64         `yaml:"max_file_bytes"`
	BatchSize    int           `yaml:"batch_size"`
	Timeout      time.Duration `yaml:"timeout"`
}

func DefaultPolicy() Policy {
	return Policy{MaxDepth: 64, MaxFiles: 1000000, MaxFileBytes: 32 << 30, BatchSize: 128, Timeout: 6 * time.Hour}
}

func (policy Policy) Validate() error {
	if policy.MaxDepth < 1 || policy.MaxDepth > 256 || policy.MaxFiles < 1 || policy.MaxFiles > 10000000 || policy.MaxFileBytes < 1 || policy.MaxFileBytes > 1<<40 || policy.BatchSize < 1 || policy.BatchSize > 256 || policy.Timeout < time.Second || policy.Timeout > 24*time.Hour {
		return ErrPolicy
	}
	return nil
}

type File struct {
	RelativePath     string
	SizeBytes        int64
	MtimeNS          int64
	Identity         string
	QuickFingerprint string
	SHA256           string
}

type Summary struct {
	Files       int64
	Bytes       int64
	Directories int64
}

// PathError keeps the normalized relative path for private administrative
// diagnostics. Public job responses use a fixed safe code, never the raw Err.
type PathError struct {
	Path string
	Err  error
}

func (err *PathError) Error() string { return fmt.Sprintf("inventory %q: %v", err.Path, err.Err) }
func (err *PathError) Unwrap() error { return err.Err }
