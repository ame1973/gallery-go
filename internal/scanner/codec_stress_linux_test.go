//go:build linux && (amd64 || arm64)

package scanner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"gallerygo/internal/media"
	"gallerygo/internal/sandbox"
)

func TestRepeatedOwnedAVIFPrivateChildrenKeepDefaultBudgets(t *testing.T) {
	policy, err := media.DefaultInspectPolicy().PinCodecRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ vector, command, pin string }{
		{"mirrored", sandbox.ChildCommand, policy.CodecRuntimePin()},
		{"infinite", sandbox.ChildCommand, policy.CodecRuntimePin()},
		{"grid", sandbox.ImageHeaderCommand, policy.ImageRuntimePin()},
		{"still", sandbox.ImageHeaderCommand, policy.ImageRuntimePin()},
	} {
		t.Run(item.vector, func(t *testing.T) {
			data := publicationAVIF(t, item.vector)
			if item.vector == "still" {
				copy(data[8:12], "mif1")
			}
			path := filepath.Join(t.TempDir(), "read-only")
			if err := os.WriteFile(path, data, 0o400); err != nil {
				t.Fatal(err)
			}
			input, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			for iteration := range 40 {
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				command := exec.CommandContext(ctx, "/proc/self/exe", item.command, "536870912", "10", "34359738368", item.pin)
				command.ExtraFiles = []*os.File{input}
				command.Env = []string{"LANG=C", "LC_ALL=C", "TZ=UTC", "HOME=/nonexistent", "OMP_NUM_THREADS=1", "OPENBLAS_NUM_THREADS=1", "MKL_NUM_THREADS=1", "VIPS_CONCURRENCY=1", "VIPS_NOVECTOR=1"}
				command.Dir = "/"
				command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL, Setpgid: true}
				command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
				command.WaitDelay = time.Second
				output, err := command.CombinedOutput()
				cancel()
				if err != nil {
					t.Fatalf("owned default-budget private child failed: profile=%s iteration=%d error=%v\n%s", item.command, iteration, err, output)
				}
			}
		})
	}
}
