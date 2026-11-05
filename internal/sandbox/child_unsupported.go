//go:build !linux || (!amd64 && !arm64)

package sandbox

import (
	"os"
	"syscall"
)

// No unsupported platform silently falls back to unrestricted execution.
func RunChild(_ []string) int               { return 70 }
func childAttributes() *syscall.SysProcAttr { return nil }
func killChild(process *os.Process) error   { return process.Kill() }
