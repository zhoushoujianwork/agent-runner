//go:build !windows

package host

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	runner "github.com/zhoushoujianwork/agent-runner/runner"
)

func TestCancelTerminatesProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	process, err := New(WithTerminationGrace(20*time.Millisecond)).Start(context.Background(), runner.CommandSpec{
		Argv: []string{"/bin/sh", "-c", `sleep 30 & echo $! > "$PIDFILE"; wait`},
		Env:  map[string]string{"PIDFILE": pidFile},
	})
	if err != nil {
		t.Fatal(err)
	}
	childPID := waitForPID(t, pidFile)
	if err := process.Cancel(); err != nil {
		t.Fatal(err)
	}
	status, err := process.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if status.ExitCode == 0 {
		t.Fatalf("cancelled process exited successfully: %+v", status)
	}
	deadline := time.Now().Add(time.Second)
	for {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d survived process-group cancellation: %v", childPID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr != nil {
				t.Fatal(convErr)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("child pid was not written")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
