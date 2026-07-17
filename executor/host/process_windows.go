//go:build windows

package host

import (
	"context"
	"os"
	"os/exec"
	"time"

	runner "github.com/zhoushoujianwork/agent-runner/runner"
)

// StartPTY is not supported on Windows in this milestone (ConPTY is future
// work); it returns ErrBackendUnsupported so OpenTerm degrades cleanly.
func (e *Executor) StartPTY(_ context.Context, _ runner.CommandSpec, _ runner.TermSize) (runner.PTYProcess, error) {
	return nil, runner.ErrBackendUnsupported
}

func configureProcess(_ *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd, _ time.Duration, done <-chan struct{}) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
		return cmd.Process.Kill()
	}
}

func processSignal(_ *os.ProcessState) string { return "" }
