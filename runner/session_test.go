package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/zhoushoujianwork/agent-runner/engine/claude"
	"github.com/zhoushoujianwork/agent-runner/executor/host"
	"github.com/zhoushoujianwork/agent-runner/runner"
)

func sessionRunner(t *testing.T) *runner.Runner {
	t.Helper()
	return &runner.Runner{
		Engine:   claude.New(buildFakeClaude(t)),
		Executor: host.New(host.WithTerminationGrace(50 * time.Millisecond)),
	}
}

func openSession(t *testing.T, req runner.SessionRequest) *runner.Session {
	t.Helper()
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	if _, ok := req.Env["FAKE_MODE"]; !ok {
		req.Env["FAKE_MODE"] = "session"
	}
	session, err := sessionRunner(t).OpenSession(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestSessionMultiTurn(t *testing.T) {
	session := openSession(t, runner.SessionRequest{CloseGrace: time.Second})

	select {
	case <-session.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("session never became ready")
	}

	for i := 1; i <= 3; i++ {
		turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: fmt.Sprintf("turn %d", i)})
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		var eventTypes []runner.EventType
		for event := range turn.Events() {
			eventTypes = append(eventTypes, event.Type)
		}
		result, err := turn.Wait()
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		expected := fmt.Sprintf("echo %d: turn %d", i, i)
		if !result.Success || result.Text != expected || result.SessionID != "sess-live" || result.Subtype != "success" {
			t.Fatalf("turn %d result: %+v", i, result)
		}
		if result.Usage.OutputTokens != 5 {
			t.Fatalf("turn %d usage not per-turn: %+v", i, result.Usage)
		}
		for _, want := range []runner.EventType{runner.EventText, runner.EventUsage, runner.EventResult} {
			if !slices.Contains(eventTypes, want) {
				t.Fatalf("turn %d events %v missing %s", i, eventTypes, want)
			}
		}
		if i == 1 && !slices.Contains(eventTypes, runner.EventInit) {
			t.Fatalf("first turn should carry the buffered init event: %v", eventTypes)
		}
		if i > 1 && slices.Contains(eventTypes, runner.EventInit) {
			t.Fatalf("init event duplicated on turn %d: %v", i, eventTypes)
		}
	}

	pid := session.PID()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.Exit(); err != nil {
		t.Fatalf("clean close should not error: %v", err)
	}
	if pid > 0 && syscall.Kill(pid, 0) == nil {
		t.Fatalf("process %d still alive after Close", pid)
	}
}

func TestSessionBusyAndClosed(t *testing.T) {
	session := openSession(t, runner.SessionRequest{
		Env:        map[string]string{"FAKE_SESSION_HANG_TURN": "1"},
		CloseGrace: 20 * time.Millisecond,
	})
	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Send(context.Background(), runner.TurnInput{Prompt: "second"})
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorBusy {
		t.Fatalf("expected busy error, got %#v", err)
	}
	_ = session.Close()
	if _, err := turn.Wait(); err == nil {
		t.Fatal("turn interrupted by Close should error")
	}
	_, err = session.Send(context.Background(), runner.TurnInput{Prompt: "after close"})
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorClosed {
		t.Fatalf("expected closed error, got %#v", err)
	}
}

// A stalling turn (agent alive but producing no result) hits the idle timeout,
// the runner interrupts it, and the session survives for the next turn.
func TestSessionTurnIdleTimeoutInterruptsWithoutKilling(t *testing.T) {
	session := openSession(t, runner.SessionRequest{
		Env:             map[string]string{"FAKE_SESSION_STALL_TURN": "1"},
		TurnIdleTimeout: 150 * time.Millisecond,
	})
	select {
	case <-session.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("session never became ready")
	}

	started := time.Now()
	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "stall"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = turn.Wait()
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorIdleTimeout {
		t.Fatalf("expected idle timeout, got %#v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("idle timeout took too long")
	}
	for range turn.Events() {
	}

	if !session.Alive() {
		t.Fatal("an interruptible turn must not kill the session")
	}
	next, err := session.Send(context.Background(), runner.TurnInput{Prompt: "still here"})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := next.Wait(); err != nil || !result.Success || result.Text != "echo 2: still here" {
		t.Fatalf("session unusable after interrupted turn: %+v %v", result, err)
	}
}

// A hung turn (agent ignores the interrupt frame) escalates to killing the
// process after CloseGrace.
func TestSessionTurnHangEscalatesToKill(t *testing.T) {
	session := openSession(t, runner.SessionRequest{
		Env:             map[string]string{"FAKE_SESSION_HANG_TURN": "2"},
		TurnIdleTimeout: 150 * time.Millisecond,
		CloseGrace:      100 * time.Millisecond,
	})
	select {
	case <-session.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("session never became ready")
	}
	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}

	turn, err = session.Send(context.Background(), runner.TurnInput{Prompt: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = turn.Wait()
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorIdleTimeout {
		t.Fatalf("expected idle timeout, got %#v", err)
	}
	select {
	case <-session.Dead():
	case <-time.After(5 * time.Second):
		t.Fatal("an unanswered interrupt must retire the session process")
	}
	for range turn.Events() {
	}
}

func TestSessionCrashMidTurn(t *testing.T) {
	session := openSession(t, runner.SessionRequest{
		Env: map[string]string{"FAKE_SESSION_CRASH_TURN": "2"},
	})
	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "fine"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(); err != nil {
		t.Fatal(err)
	}

	turn, err = session.Send(context.Background(), runner.TurnInput{Prompt: "boom"})
	if err != nil {
		t.Fatal(err)
	}
	var sawPartial bool
	for event := range turn.Events() {
		if event.Type == runner.EventText && event.Text == "partial work before dying" {
			sawPartial = true
		}
	}
	result, err := turn.Wait()
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorProcess {
		t.Fatalf("expected process error, got %#v", err)
	}
	if runErr.ExitCode != 3 || result.ExitCode != 3 {
		t.Fatalf("exit code not surfaced: err=%+v result=%+v", runErr, result)
	}
	if !sawPartial {
		t.Fatal("partial mid-turn events were lost")
	}

	_, err = session.Send(context.Background(), runner.TurnInput{Prompt: "again"})
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorProcess {
		t.Fatalf("send on dead session should fail with process error, got %#v", err)
	}
}

// Cancelling a turn's context interrupts the turn but keeps the session
// usable when the agent honours the interrupt.
func TestSessionTurnCtxCancelKeepsSessionAlive(t *testing.T) {
	session := openSession(t, runner.SessionRequest{
		Env: map[string]string{"FAKE_SESSION_STALL_TURN": "1"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	turn, err := session.Send(ctx, runner.TurnInput{Prompt: "stall"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = turn.Wait()
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorCancelled {
		t.Fatalf("expected cancelled, got %#v", err)
	}
	if !session.Alive() {
		t.Fatal("a cancelled turn must not kill an interruptible session")
	}
	next, err := session.Send(context.Background(), runner.TurnInput{Prompt: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := next.Wait(); err != nil || !result.Success {
		t.Fatalf("session unusable after cancelled turn: %+v %v", result, err)
	}
}

func TestSessionMaxTurnsSubtype(t *testing.T) {
	session := openSession(t, runner.SessionRequest{
		Env: map[string]string{"FAKE_SESSION_MAXTURNS_TURN": "1"},
	})
	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "spin"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if result.Subtype != "error_max_turns" {
		t.Fatalf("subtype not surfaced: %+v", result)
	}
	if !session.Alive() {
		t.Fatal("an error_max_turns turn must not kill the session")
	}
	turn, err = session.Send(context.Background(), runner.TurnInput{Prompt: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := turn.Wait(); err != nil || !result.Success {
		t.Fatalf("session unusable after max-turns turn: %+v %v", result, err)
	}
}

func TestSessionPermissionAllow(t *testing.T) {
	var prompts atomic.Int32
	session := openSession(t, runner.SessionRequest{
		Env:             map[string]string{"FAKE_SESSION_PERMISSION_TURN": "1"},
		TurnIdleTimeout: 500 * time.Millisecond,
		OnPermission: func(ctx context.Context, req runner.PermissionRequest) (runner.PermissionDecision, error) {
			prompts.Add(1)
			if req.ToolName != "Bash" {
				t.Errorf("unexpected tool: %+v", req)
			}
			var input struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal(req.Input, &input); err != nil || input.Command != "ls" {
				t.Errorf("unexpected input: %s", req.Input)
			}
			// Outlive the idle window to prove the idle timer pauses while a
			// permission prompt is pending.
			time.Sleep(time.Second)
			return runner.PermissionDecision{Allow: true}, nil
		},
	})
	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "use the tool"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Text != "tool approved" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if prompts.Load() != 1 {
		t.Fatalf("permission callback fired %d times", prompts.Load())
	}
}

func TestSessionPermissionDeny(t *testing.T) {
	session := openSession(t, runner.SessionRequest{
		Env: map[string]string{"FAKE_SESSION_PERMISSION_TURN": "1"},
		OnPermission: func(ctx context.Context, req runner.PermissionRequest) (runner.PermissionDecision, error) {
			return runner.PermissionDecision{Message: "nope denied"}, nil
		},
	})
	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "use the tool"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := turn.Wait()
	if err == nil || result.Success {
		t.Fatalf("denied tool use should fail the turn: %+v", result)
	}
	if !strings.Contains(err.Error(), "nope denied") {
		t.Fatalf("deny message not surfaced: %v", err)
	}
	if !session.Alive() {
		t.Fatal("a denied permission must not kill the session")
	}
	next, err := session.Send(context.Background(), runner.TurnInput{Prompt: "carry on"})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := next.Wait(); err != nil || !result.Success || result.Text != "echo 2: carry on" {
		t.Fatalf("session unusable after denied turn: %+v %v", result, err)
	}
}

// Extra dirs: a context root's .claude content is discovered and merged into
// the working directory for the process lifetime, then cleaned up on exit.
func TestSessionExtraDirs(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, ".claude", "skills", "review")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	session := openSession(t, runner.SessionRequest{
		WorkDir:   work,
		ExtraDirs: []runner.ExtraDir{{Source: root}},
	})
	link := filepath.Join(work, ".claude", "skills", "review")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("skill not linked: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("target is not a symlink: %v", info.Mode())
	}
	if data, err := os.ReadFile(filepath.Join(link, "SKILL.md")); err != nil || string(data) != "skill" {
		t.Fatalf("link does not resolve to source content: %q %v", data, err)
	}

	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := turn.Wait(); err != nil || !result.Success {
		t.Fatalf("turn failed: %+v %v", result, err)
	}

	_ = session.Close()
	<-session.Dead()
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("link must be removed after process exit: %v", err)
	}
}

// CloseInput ends the input half only: no further turns, but the process
// winds down naturally and a clean EOF exit is not an error.
func TestSessionCloseInput(t *testing.T) {
	session := openSession(t, runner.SessionRequest{})
	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := turn.Wait(); err != nil || !result.Success {
		t.Fatalf("first turn failed: %+v %v", result, err)
	}

	if err := session.CloseInput(); err != nil {
		t.Fatal(err)
	}
	if err := session.CloseInput(); err != nil {
		t.Fatalf("CloseInput must be idempotent: %v", err)
	}
	_, err = session.Send(context.Background(), runner.TurnInput{Prompt: "two"})
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorClosed {
		t.Fatalf("send after CloseInput should fail closed, got %#v", err)
	}
	select {
	case <-session.Dead():
	case <-time.After(5 * time.Second):
		t.Fatal("process should exit after stdin EOF")
	}
	if _, err := session.Exit(); err != nil {
		t.Fatalf("clean EOF exit should not error: %v", err)
	}
}

func TestSessionWaitDoesNotRequireEventConsumer(t *testing.T) {
	session := openSession(t, runner.SessionRequest{})
	turn, err := session.Send(context.Background(), runner.TurnInput{Prompt: "no consumer"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, waitErr := turn.Wait()
		done <- waitErr
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn Wait blocked because Events was not consumed")
	}
}
