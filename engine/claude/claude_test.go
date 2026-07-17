package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/agent-runner/runner"
)

func TestBuildCommand(t *testing.T) {
	protocol, err := New("fake-claude").NewSession(runner.SessionRequest{
		WorkDir:         "/workspace",
		Model:           "sonnet",
		ResumeSessionID: "sess-1",
		MaxTurns:        4,
		Permission:      runner.PermissionBypass,
		AllowedTools:    []string{"Read", "Grep"},
		Env:             map[string]string{"X": "1"},
		OnPermission: func(context.Context, runner.PermissionRequest) (runner.PermissionDecision, error) {
			return runner.PermissionDecision{Allow: true}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := protocol.Command()
	joined := strings.Join(spec.Argv, " ")
	for _, expected := range []string{
		"fake-claude", "--print",
		"--input-format stream-json", "--output-format stream-json",
		"--resume sess-1", "--permission-mode bypassPermissions",
		"--allowedTools Read,Grep", "--permission-prompt-tool stdio",
	} {
		if !strings.Contains(joined, expected) {
			t.Errorf("argv %q missing %q", joined, expected)
		}
	}
	if !spec.Interactive {
		t.Fatal("session command must be interactive")
	}
	if spec.Dir != "/workspace" || spec.Env["X"] != "1" {
		t.Fatalf("unexpected spec: %+v", spec)
	}
}

func TestEncodeTurn(t *testing.T) {
	protocol, err := New("claude").NewSession(runner.SessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.EncodeTurn(runner.TurnInput{Prompt: "secret prompt"})
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "user" || frame.Message.Role != "user" || len(frame.Message.Content) != 1 || frame.Message.Content[0].Text != "secret prompt" {
		t.Fatalf("unexpected turn frame: %s", payload)
	}
	if payload[len(payload)-1] != '\n' {
		t.Fatal("turn frame must be newline-terminated")
	}
	if _, err := protocol.EncodeTurn(runner.TurnInput{Prompt: "  "}); err == nil {
		t.Fatal("expected empty prompt error")
	}
}

func TestGoldenStream(t *testing.T) {
	protocol, err := New("claude").NewSession(runner.SessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open("testdata/stream-success.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var types []runner.EventType
	endOfTurn := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		step, err := protocol.ParseLine(scanner.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		endOfTurn = step.EndOfTurn
		for _, event := range step.Events {
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
	if !endOfTurn {
		t.Fatal("result frame must end the turn")
	}
	result, err := protocol.TurnResult(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success || result.Text != "parsed" || result.SessionID != "sess-golden" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 3 || result.Usage.CostUSD != 0.01 {
		t.Fatalf("unexpected usage: %+v", result.Usage)
	}

	// The accumulator must reset between turns.
	if follow, err := protocol.TurnResult(time.Second); err == nil || follow.Success {
		t.Fatalf("second TurnResult must fail without a new result frame: %+v", follow)
	}
}

func TestInvalidSessionRequest(t *testing.T) {
	if _, err := New("claude").NewSession(runner.SessionRequest{ResumeSessionID: "s", NewSessionID: "new"}); err == nil {
		t.Fatal("expected resume/new-session error")
	}
	if _, err := New("claude").NewSession(runner.SessionRequest{ResumeSessionID: "s", Continue: true}); err == nil {
		t.Fatal("expected resume/continue error")
	}
}

func TestEncodeInterrupt(t *testing.T) {
	protocol, err := New("claude").NewSession(runner.SessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := protocol.EncodeInterrupt()
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "control_request" || frame.Request.Subtype != "interrupt" || frame.RequestID == "" {
		t.Fatalf("unexpected interrupt frame: %s", payload)
	}
}

func TestControlRequestRoundTrip(t *testing.T) {
	protocol, err := New("claude").NewSession(runner.SessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"control_request","request_id":"perm-9","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`
	step, err := protocol.ParseLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if step.Control == nil || step.Control.ID != "perm-9" || step.Control.ToolName != "Bash" {
		t.Fatalf("unexpected control step: %+v", step)
	}
	if step.EndOfTurn || len(step.Events) != 0 {
		t.Fatalf("control request must not emit events or end the turn: %+v", step)
	}

	// Allow without UpdatedInput echoes the remembered original input.
	payload, err := protocol.EncodePermissionResponse("perm-9", runner.PermissionDecision{Allow: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"behavior":"allow"`) || !strings.Contains(string(payload), `"command":"ls"`) {
		t.Fatalf("allow response lost the original input: %s", payload)
	}

	payload, err = protocol.EncodePermissionResponse("perm-9", runner.PermissionDecision{Message: "not now"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"behavior":"deny"`) || !strings.Contains(string(payload), "not now") {
		t.Fatalf("unexpected deny response: %s", payload)
	}
}

func TestUnsupportedControlRequestAnsweredInline(t *testing.T) {
	protocol, err := New("claude").NewSession(runner.SessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	step, err := protocol.ParseLine([]byte(`{"type":"control_request","request_id":"x-1","request":{"subtype":"mystery"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if step.Control != nil || len(step.Reply) == 0 {
		t.Fatalf("unsupported control request must be answered inline: %+v", step)
	}
	if !strings.Contains(string(step.Reply), `"request_id":"x-1"`) || !strings.Contains(string(step.Reply), `"subtype":"error"`) {
		t.Fatalf("unexpected inline reply: %s", step.Reply)
	}
}
