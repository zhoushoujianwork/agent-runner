package runner_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"testing"
	"time"

	runner "github.com/zhoushoujianwork/agent-runner"
	"github.com/zhoushoujianwork/agent-runner/engine/claude"
	"github.com/zhoushoujianwork/agent-runner/executor/host"
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

func TestSessionTurnIdleTimeout(t *testing.T) {
	session := openSession(t, runner.SessionRequest{
		Env:             map[string]string{"FAKE_SESSION_HANG_TURN": "2"},
		TurnIdleTimeout: 150 * time.Millisecond,
	})
	// The idle clock only measures output gaps, but a cold process may need
	// longer than one idle window just to start — wait for init like real
	// callers (prewarm) do before sending the first turn.
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

	started := time.Now()
	turn, err = session.Send(context.Background(), runner.TurnInput{Prompt: "stall"})
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
	select {
	case <-session.Dead():
	case <-time.After(5 * time.Second):
		t.Fatal("idle timeout must retire the session process")
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

func TestSessionTurnCtxCancel(t *testing.T) {
	session := openSession(t, runner.SessionRequest{
		Env: map[string]string{"FAKE_SESSION_HANG_TURN": "1"},
	})
	ctx, cancel := context.WithCancel(context.Background())
	turn, err := session.Send(ctx, runner.TurnInput{Prompt: "hang"})
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
	select {
	case <-session.Dead():
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling a turn must retire the session process")
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
