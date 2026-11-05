// Package sandbox runs one fixed metadata tool against an already confined input.
// It never accepts API commands, source paths, environment overrides or a shell.
package sandbox

import (
	"errors"
	"time"
)

var (
	ErrUnavailable    = errors.New("required codec isolation is unavailable")
	ErrLimits         = errors.New("invalid codec resource limits")
	ErrInput          = errors.New("codec input is not a bounded read-only regular file")
	ErrResource       = errors.New("codec processing exceeded its resource budget")
	ErrProbe          = errors.New("codec metadata could not be inspected")
	ErrRuntimeChanged = errors.New("codec runtime changed from its pinned installation")
)

const ChildCommand = "__codec-probe"

// Limits are trusted worker policy, never command-line options from an API caller.
// The child independently validates them before establishing irreversible limits.
type Limits struct {
	MemoryBytes uint64
	CPUSeconds  uint64
	InputBytes  int64
	OutputBytes int64
	Timeout     time.Duration
}

func DefaultLimits() Limits {
	return Limits{512 << 20, 10, 32 << 30, 1 << 20, 15 * time.Second}
}

func validRuntimeSignature(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, digit := range value {
		if !((digit >= '0' && digit <= '9') || (digit >= 'a' && digit <= 'f')) {
			return false
		}
	}
	return true
}

func (limits Limits) Validate() error {
	if limits.MemoryBytes < 128<<20 || limits.MemoryBytes > 2<<30 || limits.CPUSeconds < 1 || limits.CPUSeconds > 60 || limits.InputBytes < 1 || limits.InputBytes > 1<<40 || limits.OutputBytes < 1024 || limits.OutputBytes > 8<<20 || limits.Timeout < time.Second || limits.Timeout > time.Minute {
		return ErrLimits
	}
	return nil
}
