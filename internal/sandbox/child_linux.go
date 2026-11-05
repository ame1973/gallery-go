//go:build linux && (amd64 || arm64)

package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const probeBinary = "/usr/bin/ffprobe"

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
func RunChild(args []string) int {
	if len(args) != 3 {
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
		"-probesize", "8388608", "-analyzeduration", "5000000", "-protocol_whitelist", "file,pipe",
		"-format_whitelist", "mov,matroska,webm,image2,image2pipe,jpeg_pipe,png_pipe,webp_pipe,gif,tiff_pipe",
		"-show_entries", "stream=index,codec_type,codec_name,width,height,duration,avg_frame_rate,nb_frames:stream_tags=rotate,creation_time:stream_side_data=rotation:format=format_name,duration:format_tags=creation_time",
		"-of", "json", "-i", "/proc/self/fd/3"}
	// Landlock before ABI8 is thread-local. This trusted helper never runs codec code
	// until exec replaces every sibling Go thread; the final thread must stay locked.
	runtime.LockOSThread()
	if err := restrictFilesystem(input); err != nil {
		return 70
	}
	if err := applyLimits(limits); err != nil {
		return 70
	}
	if err := restrictSyscalls(); err != nil {
		return 70
	}
	err = unix.Exec(probeBinary, arguments, childEnvironment)
	// The Go file finalizer must not close the inherited input after the last
	// policy check but before exec; ELF resolution can trigger garbage collection.
	runtime.KeepAlive(input)
	if err != nil {
		return 70
	}
	return 70
}

func applyLimits(limits Limits) error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return err
	}
	capabilities := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capset(&capabilities, &data[0]); err != nil {
		return err
	}
	for resource, limit := range map[int]unix.Rlimit{
		unix.RLIMIT_AS:     {Cur: limits.MemoryBytes, Max: limits.MemoryBytes},
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

func restrictFilesystem(input *os.File) error {
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
	files, err := runtimeFiles()
	if err != nil {
		return err
	}
	probe, err := filepath.EvalSymlinks(probeBinary)
	if err != nil {
		return err
	}
	loader, err := filepath.EvalSymlinks(dynamicLoader)
	if err != nil {
		return err
	}
	if _, err := os.Stat("/etc/ld.so.cache"); err == nil {
		files = append(files, "/etc/ld.so.cache")
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, name := range files {
		rights := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE)
		if name == probe || name == loader {
			rights |= unix.LANDLOCK_ACCESS_FS_EXECUTE
		}
		parent, err := unix.Open(name, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		err = add(parent, rights)
		unix.Close(parent)
		if err != nil {
			return err
		}
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func restrictSyscalls() error {
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
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	result, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		return errno
	}
	if result != 0 {
		return ErrUnavailable
	}
	return nil
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
