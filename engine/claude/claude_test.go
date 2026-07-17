package claude

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	runner "github.com/zhoushoujianwork/agent-runner"
)

func TestBuildCommand(t *testing.T) {
	run, err := New("fake-claude").NewRun(runner.Request{
		Prompt:       "secret prompt",
		WorkDir:      "/workspace",
		Model:        "sonnet",
		SessionID:    "sess-1",
		MaxTurns:     4,
		Permission:   runner.PermissionBypass,
		AllowedTools: []string{"Read", "Grep"},
		Env:          map[string]string{"X": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := run.Command()
	joined := strings.Join(spec.Argv, " ")
	for _, expected := range []string{"fake-claude", "--print", "--output-format stream-json", "--resume sess-1", "--permission-mode bypassPermissions", "--allowedTools Read,Grep"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("argv %q missing %q", joined, expected)
		}
	}
	if strings.Contains(joined, "secret prompt") {
		t.Fatal("prompt leaked into argv")
	}
	if string(spec.Stdin) != "secret prompt\n" {
		t.Fatalf("stdin = %q", spec.Stdin)
	}
	if spec.Dir != "/workspace" || spec.Env["X"] != "1" {
		t.Fatalf("unexpected spec: %+v", spec)
	}
}

func TestGoldenStream(t *testing.T) {
	run, err := New("claude").NewRun(runner.Request{Prompt: "test"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open("testdata/stream-success.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var types []runner.EventType
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		events, err := run.ParseLine(scanner.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			types = append(types, event.Type)
			if event.SessionID != "sess-golden" {
				t.Fatalf("session = %q", event.SessionID)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []runner.EventType{runner.EventInit, runner.EventTextDelta, runner.EventText, runner.EventToolUse, runner.EventToolResult, runner.EventUsage} {
		if !slices.Contains(types, expected) {
			t.Errorf("events %v missing %s", types, expected)
		}
	}
	result, err := run.Finalize(runner.ExitStatus{ExitCode: 0}, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Text != "parsed" || result.SessionID != "sess-golden" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 3 || result.Usage.CostUSD != 0.01 {
		t.Fatalf("unexpected usage: %+v", result.Usage)
	}
}

func TestInvalidRequest(t *testing.T) {
	if _, err := New("claude").NewRun(runner.Request{}); err == nil {
		t.Fatal("expected empty prompt error")
	}
	if _, err := New("claude").NewRun(runner.Request{Prompt: "x", SessionID: "s", Continue: true}); err == nil {
		t.Fatal("expected resume/continue error")
	}
	if _, err := New("claude").NewRun(runner.Request{Prompt: "x", SessionID: "s", NewSessionID: "new"}); err == nil {
		t.Fatal("expected resume/new-session error")
	}
}
