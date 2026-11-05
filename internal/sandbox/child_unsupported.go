//go:build !linux || (!amd64 && !arm64)

package sandbox

import (
	"context"
	"os"
	"syscall"
)

// No unsupported platform silently falls back to unrestricted execution.
func Signature(_ context.Context) (string, error) { return "", ErrUnavailable }
func RunChild(_ []string) int                     { return 70 }
func childAttributes() *syscall.SysProcAttr       { return nil }
func killChild(process *os.Process) error         { return process.Kill() }
