//go:build linux && (amd64 || arm64)

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == ImageHeaderCommand {
		os.Exit(RunImageChild(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == ChildCommand {
		if status := supervisionTestChild(); status >= 0 {
			os.Exit(status)
		}
		os.Exit(RunChild(os.Args[2:]))
	}
	if len(os.Args) > 2 && os.Args[1] == "__sandbox-budget-test" {
		exitRestrictedTest(sandboxBudgetTest(os.Args[2]))
	}
	if len(os.Args) > 2 && os.Args[1] == "__sandbox-unavailable-test" {
		exitRestrictedTest(unavailableIsolationTest(os.Args[2]))
	}
	if len(os.Args) > 3 && os.Args[1] == "__sandbox-image-self-test" {
		os.Exit(sandboxSelfTestFor(os.Args[2], profileImageHeader))
	}
	if len(os.Args) > 2 && os.Args[1] == "__sandbox-self-test" {
		os.Exit(sandboxSelfTest(os.Args[2]))
	}
}

func testSignature(t *testing.T) string {
	t.Helper()
	pin, err := Signature(t.Context())
	if err != nil {
		t.Fatal("fixed codec runtime cannot be pinned", err)
	}
	return pin
}

func testProbe(t *testing.T, ctx context.Context, input *os.File, limits Limits) ([]byte, error) {
	t.Helper()
	return Probe(ctx, input, limits, testSignature(t))
}

func probeInput(t *testing.T) *os.File {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewNRGBA(image.Rect(0, 0, 12, 7))); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "input.png")
	if err := os.WriteFile(name, encoded.Bytes(), 0o400); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close() })
	return input
}

func TestFixedFFprobeIsActuallyConfinedAndReturnsOnlyProjectedMetadata(t *testing.T) {
	input := probeInput(t)
	output, err := testProbe(t, t.Context(), input, DefaultLimits())
	if err != nil {
		binary, _ := os.Executable()
		command := exec.CommandContext(t.Context(), binary, ChildCommand, "536870912", "10", "34359738368", testSignature(t))
		command.ExtraFiles, command.Env = []*os.File{input}, childEnvironment
		diagnostic, failure := command.CombinedOutput()
		t.Fatalf("actual required fixed codec sandbox failed: %v; child=%v\n%s", err, failure, diagnostic)
	}
	var metadata struct {
		Streams []struct {
			Codec  string `json:"codec_name"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(output, &metadata); err != nil || len(metadata.Streams) != 1 || metadata.Streams[0].Codec != "png" || metadata.Streams[0].Width != 12 || metadata.Streams[0].Height != 7 || bytes.Contains(output, []byte(input.Name())) || bytes.Contains(output, []byte("filename")) {
		t.Fatal("invalid or unprojected tool output", string(output), err)
	}
}

func TestSandboxInputLimitsAndCancellationFailClosed(t *testing.T) {
	input := probeInput(t)
	limits := DefaultLimits()
	limits.InputBytes = 1
	if _, err := testProbe(t, t.Context(), input, limits); !errors.Is(err, ErrInput) {
		t.Fatal("oversize input accepted", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := testProbe(t, ctx, input, DefaultLimits()); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled probe started", err)
	}
	writable, err := os.OpenFile(filepath.Join(t.TempDir(), "writable"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	if _, err := writable.WriteString("not readonly"); err != nil {
		t.Fatal(err)
	}
	if _, err := testProbe(t, t.Context(), writable, DefaultLimits()); !errors.Is(err, ErrInput) {
		t.Fatal("writable descriptor accepted", err)
	}
	pathFD, err := unix.Open(input.Name(), unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	pathOnly := os.NewFile(uintptr(pathFD), "path-only")
	defer pathOnly.Close()
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	empty, err := os.CreateTemp(t.TempDir(), "empty")
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	closed := probeInput(t)
	_ = closed.Close()
	for _, candidate := range []*os.File{nil, pathOnly, directory, empty, closed} {
		if _, err := testProbe(t, t.Context(), candidate, DefaultLimits()); !errors.Is(err, ErrInput) {
			t.Fatal("invalid input authority accepted", candidate, err)
		}
	}
}

func TestLandlockAndSeccompActuallyDenySiblingReadWritesNetworkAndProcesses(t *testing.T) {
	input := probeInput(t)
	outside := filepath.Join(t.TempDir(), "outside-canary")
	if err := os.WriteFile(outside, []byte("private sibling canary"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(input.Name(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"__sandbox-self-test", "__sandbox-image-self-test"} {
		command := exec.CommandContext(t.Context(), binary, mode, outside, "assert")
		command.ExtraFiles = []*os.File{input}
		command.Env = childEnvironment
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("actual confinement assertions failed (%s): %v\n%s", mode, err, output)
		}
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "private sibling canary" {
		t.Fatal("sandbox changed sibling canary", err)
	}
}

func sandboxSelfTest(outside string) int { return sandboxSelfTestFor(outside, profileProbe) }

func sandboxSelfTestFor(outside string, profile codecProfile) int {
	pin, err := signatureFor(context.Background(), profile)
	if err != nil {
		return 70
	}
	unusedModule := ""
	if profile == profileImageHeader {
		for _, directory := range libraryDirectories {
			matches, _ := filepath.Glob(filepath.Join(directory, "vips-modules-*/vips-magick.so"))
			if len(matches) > 0 {
				unusedModule = matches[0]
				break
			}
		}
	}
	parent := os.Getppid()
	runtime.LockOSThread()
	input := os.NewFile(3, "input")
	if restrictFilesystemFor(input, pin, profile) != nil || restrictSyscalls() != nil {
		return 70
	}
	if unusedModule != "" {
		if fd, err := unix.Open(unusedModule, unix.O_RDONLY, 0); err == nil {
			unix.Close(fd)
			return 94
		}
	}
	if fd, err := unix.Open(outside, unix.O_RDONLY, 0); err == nil {
		unix.Close(fd)
		return 80
	}
	if fd, err := unix.Open("/proc/self/fd/3", unix.O_WRONLY, 0); err == nil {
		unix.Close(fd)
		return 81
	}
	fd, err := unix.Open("/proc/self/fd/3", unix.O_RDONLY, 0)
	if err != nil {
		return 82
	}
	var data [8]byte
	_, err = unix.Read(fd, data[:])
	unix.Close(fd)
	if err != nil || string(data[:]) != "\x89PNG\r\n\x1a\n" {
		return 83
	}
	if fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0); err == nil {
		unix.Close(fd)
		return 84
	}
	if _, _, errno := unix.RawSyscall(unix.SYS_CLONE, 0, 0, 0); errno != unix.EPERM {
		return 85
	}
	if err := unix.Chmod(outside, 0o777); err != unix.EPERM {
		return 86
	}
	if _, err := unix.FcntlInt(input.Fd(), unix.F_SETLEASE, unix.F_RDLCK); err != unix.EPERM {
		return 87
	}
	if err := unix.Prlimit(parent, unix.RLIMIT_NOFILE, nil, &unix.Rlimit{}); err != unix.EPERM {
		return 88
	}
	if err := unix.Tgkill(parent, parent, 0); err != unix.EPERM {
		return 89
	}
	if err := unix.Tgkill(os.Getpid(), unix.Gettid(), 0); err != nil {
		return 90
	}
	if fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM, 0); err != unix.EPERM {
		if err == nil {
			unix.Close(fd)
		}
		return 91
	}
	if err := unix.Exec("/bin/sh", []string{"sh", "-c", "exit 92"}, childEnvironment); err != unix.EACCES {
		return 93
	}
	runtime.KeepAlive(input)
	return 0
}

// These controlled children exercise parent supervision only. They are test-only,
// never a command/path override in the production helper or codec evidence.
func supervisionTestChild() int {
	var header [1024]byte
	n, err := unix.Pread(3, header[:], 0)
	if err != nil || !strings.HasPrefix(string(header[:n]), "gallerygo-supervision-test\n") {
		return -1
	}
	fields := strings.SplitN(string(header[:n]), "\n", 3)
	switch fields[1] {
	case "stdout":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 8192))
	case "stderr":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("private diagnostic"), 8192))
	case "unavailable":
		return 70
	case "invalid":
		return 71
	case "failed":
		return 1
	case "signal":
		_ = unix.Kill(os.Getpid(), unix.SIGKILL)
	case "wait":
		if len(fields) != 3 || os.WriteFile(fields[2], []byte("started"), 0o600) != nil {
			return 1
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	return 0
}

func supervisionInput(t *testing.T, mode, marker string) *os.File {
	t.Helper()
	name := filepath.Join(t.TempDir(), "supervision-input")
	if err := os.WriteFile(name, []byte("gallerygo-supervision-test\n"+mode+"\n"+marker), 0o400); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close() })
	return input
}

func TestParentBoundsBothPipesAndMapsFailuresWithoutPrivateOutput(t *testing.T) {
	for mode, expected := range map[string]error{
		"stdout": ErrResource, "stderr": ErrResource, "unavailable": ErrUnavailable,
		"invalid": ErrInput, "failed": ErrProbe, "signal": ErrResource,
	} {
		t.Run(mode, func(t *testing.T) {
			limits := DefaultLimits()
			limits.OutputBytes = 1024
			output, err := testProbe(t, t.Context(), supervisionInput(t, mode, ""), limits)
			if !errors.Is(err, expected) || len(output) != 0 {
				t.Fatal("failed child leaked output or returned incorrect error", len(output), err)
			}
		})
	}
}

func TestParentCancelsAnAlreadyRunningChildAndEnforcesWallDeadline(t *testing.T) {
	for _, activeCancel := range []bool{true, false} {
		t.Run(map[bool]string{true: "active cancellation", false: "wall deadline"}[activeCancel], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			limits := DefaultLimits()
			limits.Timeout = time.Second
			marker := filepath.Join(t.TempDir(), "started")
			input := supervisionInput(t, "wait", marker)
			result := make(chan error, 1)
			go func() {
				_, err := testProbe(t, ctx, input, limits)
				result <- err
			}()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case err := <-result:
					t.Fatal("child did not actually start", err)
				case <-ticker.C:
				case <-t.Context().Done():
					t.Fatal("test cancelled before child startup")
				}
			}
			expected := ErrResource
			if activeCancel {
				expected = context.Canceled
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, expected) {
					t.Fatal("live child cancellation/deadline failed", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("child survived cancellation/deadline")
			}
		})
	}
}

func TestActualChildResourceLimitsDoNotChangeTheParent(t *testing.T) {
	var before, after unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_AS, &before); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"limits", "cpu"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "__sandbox-budget-test", mode)
			command.ExtraFiles = []*os.File{probeInput(t)}
			command.Env = childEnvironment
			output, err := command.CombinedOutput()
			if mode == "limits" {
				if err != nil {
					t.Fatalf("child resource assertions failed: %v\n%s", err, output)
				}
				return
			}
			var failure *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &failure) {
				t.Fatal("CPU limit did not terminate the child", err, ctx.Err())
			}
			status := failure.Sys().(syscall.WaitStatus)
			if status.Signal() != syscall.SIGXCPU && status.Signal() != syscall.SIGKILL {
				t.Fatal("child failed before reaching CPU limit", status, string(output))
			}
		})
	}
	if err := unix.Getrlimit(unix.RLIMIT_AS, &after); err != nil || before != after {
		t.Fatal("child changed the parent's resource policy", before, after, err)
	}
}

func sandboxBudgetTest(mode string) int {
	pin, err := Signature(context.Background())
	if err != nil {
		return 70
	}
	input := os.NewFile(3, "input")
	limits := DefaultLimits()
	limits.CPUSeconds = 1
	if mode == "cpu" {
		// A Go race runtime reserves far more address space than FFprobe. Keep its
		// existing AS budget for the CPU-death case; the separate immediate-exit
		// case checks the exact production memory limit and actual mmap denial.
		var current unix.Rlimit
		if unix.Getrlimit(unix.RLIMIT_AS, &current) != nil {
			return 69
		}
		limits.MemoryBytes = current.Cur
	}
	runtime.LockOSThread()
	if restrictFilesystem(input, pin) != nil || applyLimits(limits) != nil || restrictSyscalls() != nil {
		return 70
	}
	for resource, expected := range map[int]unix.Rlimit{
		unix.RLIMIT_AS:  {Cur: limits.MemoryBytes, Max: limits.MemoryBytes},
		unix.RLIMIT_CPU: {Cur: 1, Max: 2}, unix.RLIMIT_CORE: {}, unix.RLIMIT_FSIZE: {},
		unix.RLIMIT_NOFILE: {Cur: 64, Max: 64},
	} {
		var actual unix.Rlimit
		if unix.Getrlimit(resource, &actual) != nil || actual != expected {
			return 80
		}
	}
	if mode == "cpu" {
		for {
			_, _, _ = unix.RawSyscall(unix.SYS_GETPID, 0, 0, 0)
		}
	}
	if data, err := unix.Mmap(-1, 0, 768<<20, unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON); err != unix.ENOMEM {
		if err == nil {
			_ = unix.Munmap(data)
		}
		return 81
	}
	if err := unix.Prlimit(0, unix.RLIMIT_AS, &unix.Rlimit{Cur: ^uint64(0), Max: ^uint64(0)}, nil); err != unix.EPERM {
		return 82
	}
	if fd, err := unix.FcntlInt(input.Fd(), unix.F_DUPFD, 64); err != unix.EINVAL {
		if err == nil {
			unix.Close(fd)
		}
		return 83
	}
	runtime.KeepAlive(input)
	return 0
}

func exitRestrictedTest(status int) {
	// Race finalization sleeps and may allocate in trusted Go background threads.
	// No Go runtime may outlive this test's deliberately smaller address budget.
	_, _, _ = unix.RawSyscall(unix.SYS_EXIT_GROUP, uintptr(status), 0, 0)
	os.Exit(127)
}

func TestMissingMandatoryKernelIsolationNeverRunsTheTool(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"landlock", "seccomp"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), binary, "__sandbox-unavailable-test", mode)
			command.ExtraFiles = []*os.File{probeInput(t)}
			command.Env = childEnvironment
			output, err := command.CombinedOutput()
			var failure *exec.ExitError
			if !errors.As(err, &failure) || failure.ExitCode() != 70 || len(output) != 0 {
				t.Fatal("tool ran without mandatory isolation", string(output), err)
			}
		})
	}
}

func unavailableIsolationTest(mode string) int {
	pin, err := Signature(context.Background())
	if err != nil {
		return 80
	}
	denied := uint32(unix.SYS_LANDLOCK_CREATE_RULESET)
	if mode == "seccomp" {
		denied = unix.SYS_SECCOMP
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: denied, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	runtime.LockOSThread()
	if unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != nil {
		return 80
	}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, 0, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		return 81
	}
	return RunChild([]string{"536870912", "10", "34359738368", pin})
}
