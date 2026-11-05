//go:build linux && arm64

package sandbox

import "golang.org/x/sys/unix"

const (
	auditArchitecture = unix.AUDIT_ARCH_AARCH64
	dynamicLoader     = "/lib/ld-linux-aarch64.so.1"
)

var libraryDirectories = []string{"/lib/aarch64-linux-gnu", "/usr/lib/aarch64-linux-gnu", "/lib64", "/usr/lib64", "/lib", "/usr/lib"}

var architectureFilter []unix.SockFilter
var architectureSyscalls []uintptr
