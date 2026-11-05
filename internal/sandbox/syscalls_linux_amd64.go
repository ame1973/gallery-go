//go:build linux && amd64

package sandbox

import "golang.org/x/sys/unix"

const (
	auditArchitecture = unix.AUDIT_ARCH_X86_64
	dynamicLoader     = "/lib64/ld-linux-x86-64.so.2"
)

var libraryDirectories = []string{"/lib/x86_64-linux-gnu", "/usr/lib/x86_64-linux-gnu", "/lib64", "/usr/lib64", "/lib", "/usr/lib"}

// The x32 ABI shares the audit architecture but uses a distinct syscall table.
var architectureFilter = []unix.SockFilter{
	{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, K: 0x40000000, Jf: 1},
	{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
}

var architectureSyscalls = []uintptr{
	unix.SYS_OPEN, unix.SYS_STAT, unix.SYS_LSTAT, unix.SYS_ACCESS, unix.SYS_READLINK,
	unix.SYS_ARCH_PRCTL, unix.SYS_GETRLIMIT, unix.SYS_EPOLL_WAIT,
}
