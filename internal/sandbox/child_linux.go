//go:build linux && (amd64 || arm64)

package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	probeBinary       = "/usr/bin/ffprobe"
	imageHeaderBinary = "/usr/bin/vipsheader"
)

func profileBinary(profile codecProfile) string {
	if profile == profileImageHeader {
		return imageHeaderBinary
	}
	return probeBinary
}

func childAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
}

func killChild(process *os.Process) error {
	err := unix.Kill(-process.Pid, unix.SIGKILL)
	if err == unix.ESRCH {
		return os.ErrProcessDone
	}
	return err
}

// RunChild is a private self-exec entrypoint. It opens no config, database, listener
// or source root, and executes only the fixed probe after all isolation succeeds.
func RunChild(args []string) int      { return runChild(args, profileProbe) }
func RunImageChild(args []string) int { return runChild(args, profileImageHeader) }

func runChild(args []string, profile codecProfile) int {
	if len(args) != 4 || !validRuntimeSignature(args[3]) {
		return 70
	}
	limits := DefaultLimits()
	var err error
	limits.MemoryBytes, err = strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return 70
	}
	limits.CPUSeconds, err = strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return 70
	}
	limits.InputBytes, err = strconv.ParseInt(args[2], 10, 64)
	if err != nil || limits.Validate() != nil {
		return 70
	}
	input := os.NewFile(3, "codec-input")
	if input == nil {
		return 71
	}
	flags, err := unix.FcntlInt(input.Fd(), unix.F_GETFL, 0)
	info, statErr := input.Stat()
	if err != nil || statErr != nil || flags&unix.O_ACCMODE != unix.O_RDONLY || flags&unix.O_PATH != 0 || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limits.InputBytes {
		return 71
	}
	arguments := []string{probeBinary, "-hide_banner", "-v", "error", "-max_alloc", "67108864", "-threads", "1",
		"-max_pixels", "1000000000", "-max_streams", "64", "-max_probe_packets", "256",
		"-probesize", "8388608", "-analyzeduration", "5000000", "-protocol_whitelist", "file,pipe",
		"-format_whitelist", "mov,matroska,webm,image2,image2pipe,jpeg_pipe,png_pipe,webp_pipe,gif,tiff_pipe",
		"-select_streams", "V:0", "-show_entries", "stream=index,codec_type,codec_name,width,height,duration:stream_disposition=attached_pic:stream_tags=rotate,creation_time,com.apple.quicktime.creationdate:stream_side_data=rotation,displaymatrix:format=format_name,duration:format_tags=creation_time,com.apple.quicktime.creationdate",
		"-of", "json", "-i", "/proc/self/fd/3"}
	if profile == profileImageHeader {
		arguments = []string{imageHeaderBinary, "--vips-concurrency=1", "--vips-cache-max=0", "--vips-cache-max-memory=0", "--vips-cache-max-files=0", "/proc/self/fd/3"}
	}
	executable, err := syscall.BytePtrFromString(profileBinary(profile))
	if err != nil {
		return 70
	}
	argv, err := syscall.SlicePtrFromStrings(arguments)
	if err != nil {
		return 70
	}
	environment, err := syscall.SlicePtrFromStrings(childEnvironment)
	if err != nil {
		return 70
	}
	filter := syscallFilter()
	// Landlock before ABI8 is thread-local. Untrusted code runs only after exec
	// replaces sibling Go threads; keep the final thread locked and quiescent.
	runtime.LockOSThread()
	runtime.GOMAXPROCS(1)
	debug.SetGCPercent(-1)
	if err := restrictFilesystemFor(input, args[3], profile); err != nil {
		if err == ErrRuntimeChanged {
			return 72
		}
		return 70
	}
	runtime.GC()
	if err := applyPreExecLimits(limits); err != nil {
		return 70
	}
	return constrainedExec(limits.MemoryBytes, filter, executable, argv, environment, input)
}

// No Go allocations, stack growth, runtime syscall transitions or cleanup may run
// after lowering AS below the existing Go mappings. Seccomp already prevents thread
// creation before exec; the locked thread retains Landlock. Prepared C strings keep
// the fixed profile, original hard NOFILE budget and input FD alive across exec.
//
//go:norace
//go:nosplit
func constrainedExec(memory uint64, filter []unix.SockFilter, executable *byte, argv, environment []*byte, input *os.File) int {
	if executable != nil && len(argv) > 0 && len(environment) > 0 && installMemoryAndFilter(memory, filter) {
		_, _, _ = unix.RawSyscall(unix.SYS_EXECVE, uintptr(unsafe.Pointer(executable)), uintptr(unsafe.Pointer(&argv[0])), uintptr(unsafe.Pointer(&environment[0])))
	}
	runtime.KeepAlive(filter)
	runtime.KeepAlive(executable)
	runtime.KeepAlive(argv)
	runtime.KeepAlive(environment)
	runtime.KeepAlive(input)
	// Exit errors must not enter allocating Go/race finalizers under codec limits.
	_, _, _ = unix.RawSyscall(unix.SYS_EXIT_GROUP, 70, 0, 0)
	return 70
}

// The immutable buffers and raw kernel calls are the only operations in this
// process-private boundary. Race instrumentation would itself allocate under AS.
//
//go:norace
//go:nosplit
func installMemoryAndFilter(memory uint64, filter []unix.SockFilter) bool {
	limit := unix.Rlimit{Cur: memory, Max: memory}
	_, _, errno := unix.RawSyscall6(unix.SYS_PRLIMIT64, 0, unix.RLIMIT_AS, uintptr(unsafe.Pointer(&limit)), 0, 0, 0)
	if errno != 0 {
		return false
	}
	result, filterErr := installSyscallFilter(filter)
	return result == 0 && filterErr == 0
}

func applyPreExecLimits(limits Limits) error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return err
	}
	capabilities := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capset(&capabilities, &data[0]); err != nil {
		return err
	}
	for resource, limit := range map[int]unix.Rlimit{
		unix.RLIMIT_CPU:    {Cur: limits.CPUSeconds, Max: limits.CPUSeconds + 1},
		unix.RLIMIT_CORE:   {Cur: 0, Max: 0},
		unix.RLIMIT_FSIZE:  {Cur: 0, Max: 0},
		unix.RLIMIT_NOFILE: {Cur: 64, Max: 64},
	} {
		if err := unix.Setrlimit(resource, &limit); err != nil {
			return err
		}
	}
	return nil
}

func restrictFilesystem(input *os.File, expected string) error {
	return restrictFilesystemFor(input, expected, profileProbe)
}

func restrictFilesystemFor(input *os.File, expected string, profile codecProfile) error {
	version, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 || version < 3 {
		return ErrUnavailable
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	// ABI3 handles every filesystem right through TRUNCATE. Unhandled newer rights
	// are covered by the syscall allowlist: no ioctl, namespace, mount or IPC calls.
	attributes := unix.LandlockRulesetAttr{Handled_access_fs: (1 << 15) - 1}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attributes)), 8, 0)
	if errno != 0 {
		return errno
	}
	defer unix.Close(int(fd))
	add := func(parent int, rights uint64) error {
		rule := unix.LandlockPathBeneathAttr{Allowed_access: rights, Parent_fd: int32(parent)}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		if errno != 0 {
			return errno
		}
		return nil
	}
	if err := add(int(input.Fd()), unix.LANDLOCK_ACCESS_FS_READ_FILE); err != nil {
		return err
	}
	files, err := runtimeFilesFor(profile)
	if err != nil {
		return err
	}
	probe, err := filepath.EvalSymlinks(profileBinary(profile))
	if err != nil {
		return err
	}
	loader, err := filepath.EvalSymlinks(dynamicLoader)
	if err != nil {
		return err
	}
	pin, err := runtimeFingerprint(context.Background(), files, func(name string, fd int) error {
		rights := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE)
		if name == probe || name == loader {
			rights |= unix.LANDLOCK_ACCESS_FS_EXECUTE
		}
		return add(fd, rights)
	})
	if err != nil {
		return err
	}
	if pin != expected {
		return ErrRuntimeChanged
	}
	if profile == profileImageHeader {
		// GLib/libheif enumerate their module folders. Directory names are readable,
		// but payload reads remain restricted to the exact pinned decoder files.
		seen := make(map[string]bool)
		for _, name := range files {
			if !strings.Contains(name, "/vips-modules-") && !strings.Contains(name, "/libheif/plugins/") {
				continue
			}
			directory := filepath.Dir(name)
			if seen[directory] {
				continue
			}
			seen[directory] = true
			fd, err := unix.Open(directory, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				return err
			}
			err = add(fd, unix.LANDLOCK_ACCESS_FS_READ_DIR)
			unix.Close(fd)
			if err != nil {
				return err
			}
		}
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func restrictSyscalls() error {
	result, errno := installSyscallFilter(syscallFilter())
	if errno != 0 {
		return errno
	}
	if result != 0 {
		return ErrUnavailable
	}
	return nil
}

func syscallFilter() []unix.SockFilter {
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: auditArchitecture, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
	filter = append(filter, architectureFilter...)
	// Go's trusted pre-exec runtime can request asynchronous preemption, but a
	// codec must never signal the worker or another same-UID process.
	selfSignal := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(os.Getpid()), Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 20},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: 0, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	filter = append(filter, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_TGKILL, Jf: uint8(len(selfSignal))})
	filter = append(filter, selfSignal...)
	// fcntl is only descriptor bookkeeping; leases, signal ownership and similar
	// commands must not let a codec affect the source or another process.
	fcntl := []unix.SockFilter{{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 24}}
	for _, command := range []uint32{unix.F_DUPFD, unix.F_DUPFD_CLOEXEC, unix.F_GETFD, unix.F_SETFD, unix.F_GETFL, unix.F_SETFL} {
		fcntl = append(fcntl,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: command, Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	}
	fcntl = append(fcntl, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)})
	filter = append(filter, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_FCNTL, Jf: uint8(len(fcntl))})
	filter = append(filter, fcntl...)
	// Only self queries are needed after limits are installed. Never permit a
	// codec to change another same-UID process's limits, or its own hard budget.
	query := make([]unix.SockFilter, 0, 13)
	for _, offset := range []uint32{16, 20, 32, 36} {
		query = append(query,
			unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: offset},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: 0, Jt: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)})
	}
	query = append(query, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	filter = append(filter, unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_PRLIMIT64, Jf: uint8(len(query))})
	filter = append(filter, query...)
	for _, number := range allowedSyscalls() {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(number), Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	}
	filter = append(filter, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)})
	return filter
}

//go:norace
//go:nosplit
func installSyscallFilter(filter []unix.SockFilter) (uintptr, unix.Errno) {
	if len(filter) == 0 || len(filter) > 4096 {
		return 0, unix.EINVAL
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	result, _, errno := unix.RawSyscall6(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	runtime.KeepAlive(filter)
	return result, errno
}

func allowedSyscalls() []uintptr {
	return append([]uintptr{
		unix.SYS_READ, unix.SYS_WRITE, unix.SYS_READV, unix.SYS_WRITEV, unix.SYS_PREAD64,
		unix.SYS_CLOSE, unix.SYS_CLOSE_RANGE, unix.SYS_FSTAT, unix.SYS_NEWFSTATAT, unix.SYS_STATX,
		unix.SYS_OPENAT, unix.SYS_OPENAT2, unix.SYS_READLINKAT, unix.SYS_FACCESSAT, unix.SYS_FACCESSAT2,
		unix.SYS_LSEEK, unix.SYS_GETDENTS64,
		unix.SYS_MMAP, unix.SYS_MPROTECT, unix.SYS_MUNMAP, unix.SYS_MREMAP, unix.SYS_MADVISE, unix.SYS_BRK,
		unix.SYS_RT_SIGACTION, unix.SYS_RT_SIGPROCMASK, unix.SYS_RT_SIGRETURN, unix.SYS_SIGALTSTACK,
		unix.SYS_EXIT, unix.SYS_EXIT_GROUP, unix.SYS_FUTEX, unix.SYS_SET_TID_ADDRESS, unix.SYS_SET_ROBUST_LIST, unix.SYS_RSEQ,
		unix.SYS_EPOLL_CREATE1, unix.SYS_EPOLL_CTL, unix.SYS_EPOLL_PWAIT, unix.SYS_EPOLL_PWAIT2, unix.SYS_EVENTFD2,
		unix.SYS_CLOCK_GETTIME, unix.SYS_CLOCK_GETRES, unix.SYS_GETTIMEOFDAY, unix.SYS_NANOSLEEP, unix.SYS_CLOCK_NANOSLEEP,
		unix.SYS_GETPID, unix.SYS_GETTID, unix.SYS_GETUID, unix.SYS_GETEUID, unix.SYS_GETGID, unix.SYS_GETEGID,
		unix.SYS_SCHED_GETAFFINITY, unix.SYS_SCHED_YIELD, unix.SYS_SYSINFO, unix.SYS_UNAME,
		unix.SYS_GETRANDOM, unix.SYS_EXECVE,
	}, architectureSyscalls...)
}
