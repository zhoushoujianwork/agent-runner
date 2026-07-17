package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/agent-runner/engine/claude"
	"github.com/zhoushoujianwork/agent-runner/executor/host"
	runner "github.com/zhoushoujianwork/agent-runner/runner"
)

func TestRunnerSuccess(t *testing.T) {
	binary := buildFakeClaude(t)
	r := &runner.Runner{Engine: claude.New(binary), Executor: host.New(host.WithTerminationGrace(50 * time.Millisecond))}
	handle, err := r.Run(context.Background(), runner.Request{Prompt: "hello", IdleTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var eventTypes []runner.EventType
	var diagnostics []string
	for event := range handle.Events() {
		eventTypes = append(eventTypes, event.Type)
		if event.Type == runner.EventDiagnostic {
			diagnostics = append(diagnostics, event.Text)
		}
	}
	result, err := handle.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.SessionID != "sess-123" || result.Text != "hello" {
		t.Fatalf("unexpected result: %+v", result)
	}
	for _, expected := range []runner.EventType{runner.EventInit, runner.EventTextDelta, runner.EventText, runner.EventToolUse, runner.EventToolResult, runner.EventUsage, runner.EventResult} {
		if !slices.Contains(eventTypes, expected) {
			t.Errorf("events %v missing %s", eventTypes, expected)
		}
	}
	joined := strings.Join(diagnostics, "\n")
	if strings.Contains(joined, "ghp_supersecret") || !strings.Contains(joined, "[REDACTED]") {
		t.Fatalf("diagnostic was not redacted: %q", joined)
	}
}

func TestRunnerProcessErrorRedactsStderr(t *testing.T) {
	binary := buildFakeClaude(t)
	r := &runner.Runner{Engine: claude.New(binary), Executor: host.New(host.WithTerminationGrace(50 * time.Millisecond))}
	handle, err := r.Run(context.Background(), runner.Request{Prompt: "fail", Env: map[string]string{"FAKE_MODE": "error"}})
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Events() {
	}
	result, err := handle.Wait()
	if err == nil || result.ExitCode != 7 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorProcess {
		t.Fatalf("unexpected error: %#v", err)
	}
	if strings.Contains(runErr.Stderr, "sk-ant-supersecret") || !strings.Contains(runErr.Stderr, "[REDACTED]") {
		t.Fatalf("stderr was not redacted: %q", runErr.Stderr)
	}
}

func TestRunnerIdleTimeout(t *testing.T) {
	binary := buildFakeClaude(t)
	r := &runner.Runner{Engine: claude.New(binary), Executor: host.New(host.WithTerminationGrace(20 * time.Millisecond))}
	started := time.Now()
	handle, err := r.Run(context.Background(), runner.Request{
		Prompt: "idle", Env: map[string]string{"FAKE_MODE": "idle"}, IdleTimeout: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Events() {
	}
	_, err = handle.Wait()
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorIdleTimeout {
		t.Fatalf("unexpected error: %#v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("idle cancellation took too long")
	}
}

func TestRunnerWallTimeout(t *testing.T) {
	binary := buildFakeClaude(t)
	r := &runner.Runner{Engine: claude.New(binary), Executor: host.New(host.WithTerminationGrace(20 * time.Millisecond))}
	handle, err := r.Run(context.Background(), runner.Request{
		Prompt: "timeout", Env: map[string]string{"FAKE_MODE": "idle"}, WallTimeout: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Events() {
	}
	_, err = handle.Wait()
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorTimeout {
		t.Fatalf("unexpected error: %#v", err)
	}
}

func TestWaitDoesNotRequireEventConsumer(t *testing.T) {
	binary := buildFakeClaude(t)
	r := &runner.Runner{Engine: claude.New(binary), Executor: host.New()}
	handle, err := r.Run(context.Background(), runner.Request{
		Prompt: "burst", Env: map[string]string{"FAKE_MODE": "burst", "FAKE_BURST": "1500"},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, waitErr := handle.Wait()
		done <- waitErr
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait blocked because Events was not consumed")
	}
	go func() {
		for range handle.Events() {
		}
	}()
}

func buildFakeClaude(t *testing.T) string {
	t.Helper()
	name := "fake-claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", path, "../internal/fakeclaude")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake claude: %v\n%s", err, output)
	}
	return path
}

func TestCommandDoesNotExposePrompt(t *testing.T) {
	binary := buildFakeClaude(t)
	argsPath := filepath.Join(t.TempDir(), "args.json")
	r := &runner.Runner{Engine: claude.New(binary), Executor: host.New()}
	handle, err := r.Run(context.Background(), runner.Request{
		Prompt: "do not expose me", Env: map[string]string{"FAKE_ARGS_PATH": argsPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	for range handle.Events() {
	}
	if _, err := handle.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "do not expose me") {
		t.Fatalf("prompt leaked into argv: %v", args)
	}
}
