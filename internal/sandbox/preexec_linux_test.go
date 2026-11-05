//go:build linux && (amd64 || arm64)

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func TestFixedProfilesExecWithDefaultBudgetsAndAggressivePreExecGC(t *testing.T) {
	input := probeInput(t)
	probePin := testSignature(t)
	imagePin := imageSignature(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range []struct{ command, pin string }{{ChildCommand, probePin}, {ImageHeaderCommand, imagePin}} {
		t.Run(profile.command, func(t *testing.T) {
			for iteration := range 20 {
				ctx, cancel := context.WithTimeout(t.Context(), DefaultLimits().Timeout)
				command := exec.CommandContext(ctx, binary, profile.command, "536870912", "10", "34359738368", profile.pin)
				command.ExtraFiles = []*os.File{input}
				command.Env = append(append([]string(nil), childEnvironment...), "GOGC=1")
				command.Dir, command.SysProcAttr = "/", childAttributes()
				command.Cancel = func() error { return killChild(command.Process) }
				output, err := command.CombinedOutput()
				cancel()
				if err != nil {
					t.Fatalf("default-budget child failed under trusted pre-exec GC pressure: iteration=%d error=%v\n%s", iteration, err, output)
				}
			}
		})
	}
}
