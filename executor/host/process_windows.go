//go:build windows

package host

import (
	"os"
	"os/exec"
	"time"
)

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
