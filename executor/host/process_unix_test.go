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

func TestExtraDirLinkLifecycle(t *testing.T) {
	work := t.TempDir()
	src := t.TempDir()
	link := filepath.Join(work, ".claude", filepath.Base(src))
	sawFile := filepath.Join(t.TempDir(), "saw")
	// The child records whether the link was visible, then exits.
	process, err := New().Start(context.Background(), runner.CommandSpec{
		Argv:      []string{"/bin/sh", "-c", `[ -L "$LINK" ] && echo yes > "$SAW"`},
		Dir:       work,
		Env:       map[string]string{"LINK": link, "SAW": sawFile},
		ExtraDirs: []runner.ExtraDir{{Source: src}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sawFile)
	if err != nil || strings.TrimSpace(string(data)) != "yes" {
		t.Fatalf("link was not visible during run: %q %v", data, err)
	}
	// After reap the link this process created must be gone.
	deadline := time.Now().Add(time.Second)
	for {
		if _, statErr := os.Lstat(link); os.IsNotExist(statErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("link not cleaned up after exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
