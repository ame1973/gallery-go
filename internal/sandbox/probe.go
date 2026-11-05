package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"
)

// Probe returns only the fixed tool's narrow JSON projection. The caller must
// still validate that metadata and rehash/bind it to the immutable source revision.
func Probe(ctx context.Context, input *os.File, limits Limits, expected string) ([]byte, error) {
	return runProbe(ctx, input, limits, expected, ChildCommand)
}

// ImageHeader returns only vipsheader's default single-line geometry summary,
// never its raw all-fields/history/extension output. Callers validate it strictly.
func ImageHeader(ctx context.Context, input *os.File, limits Limits, expected string) ([]byte, error) {
	return runProbe(ctx, input, limits, expected, ImageHeaderCommand)
}

func runProbe(ctx context.Context, input *os.File, limits Limits, expected, child string) ([]byte, error) {
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil {
		return nil, ErrInput
	}
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limits.InputBytes {
		return nil, ErrInput
	}
	if !validRuntimeSignature(expected) {
		return nil, ErrUnavailable
	}
	attributes := childAttributes()
	if attributes == nil {
		return nil, ErrUnavailable
	}
	runCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()
	// /proc/self/exe pins the executing Go image even if its pathname is replaced.
	command := exec.CommandContext(runCtx, "/proc/self/exe", child,
		strconv.FormatUint(limits.MemoryBytes, 10), strconv.FormatUint(limits.CPUSeconds, 10), strconv.FormatInt(limits.InputBytes, 10), expected)
	command.Env = childEnvironment
	command.Dir = "/"
	command.ExtraFiles = []*os.File{input}
	command.SysProcAttr = attributes
	command.Cancel = func() error { return killChild(command.Process) }
	command.WaitDelay = time.Second
	output := &boundedOutput{limit: limits.OutputBytes, cancel: cancel}
	diagnostic := &boundedOutput{limit: 64 << 10, cancel: cancel}
	command.Stdout, command.Stderr = output, diagnostic
	err = command.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if output.exceeded.Load() || diagnostic.exceeded.Load() || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, ErrResource
	}
	if err != nil {
		var failure *exec.ExitError
		if errors.As(err, &failure) {
			switch failure.ExitCode() {
			case 70:
				return nil, ErrUnavailable
			case 71:
				return nil, ErrInput
			case 72:
				return nil, ErrRuntimeChanged
			case -1:
				return nil, ErrResource
			}
		}
		return nil, ErrProbe
	}
	return output.data.Bytes(), nil
}

var childEnvironment = []string{
	"LANG=C", "LC_ALL=C", "TZ=UTC", "HOME=/nonexistent",
	"OMP_NUM_THREADS=1", "OPENBLAS_NUM_THREADS=1", "MKL_NUM_THREADS=1",
	"VIPS_CONCURRENCY=1", "VIPS_NOVECTOR=1",
}

type boundedOutput struct {
	data     bytes.Buffer
	limit    int64
	exceeded atomic.Bool
	cancel   context.CancelFunc
}

func (output *boundedOutput) Write(data []byte) (int, error) {
	if int64(len(data)) > output.limit-int64(output.data.Len()) {
		output.exceeded.Store(true)
		output.cancel()
		return 0, ErrResource
	}
	return output.data.Write(data)
}
