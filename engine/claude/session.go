package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	runner "github.com/zhoushoujianwork/agent-runner"
)

// NewSession implements runner.SessionEngine: one persistent claude process in
// realtime streaming-input mode. `--print --input-format stream-json` keeps the
// process alive across turns — each Session.Send writes one stream-json user
// frame to stdin and the turn ends at that turn's `result` frame.
func (e *Engine) NewSession(req runner.SessionRequest) (runner.SessionProtocol, error) {
	conversationModes := 0
	if req.ResumeSessionID != "" {
		conversationModes++
	}
	if req.NewSessionID != "" {
		conversationModes++
	}
	if conversationModes > 1 {
		return nil, &runner.RunError{Kind: runner.ErrorInvalidRequest, Op: "claude session build", Err: errors.New("resume_session_id and new_session_id are mutually exclusive")}
	}

	args := []string{
		e.Binary,
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
	}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", req.MaxTurns))
	}
	if req.ResumeSessionID != "" {
		args = append(args, "--resume", req.ResumeSessionID)
	} else if req.NewSessionID != "" {
		args = append(args, "--session-id", req.NewSessionID)
	}
	if req.AppendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.AppendSystemPrompt)
	}
	if len(req.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(req.AllowedTools, ","))
	}
	if len(req.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(req.DisallowedTools, ","))
	}
	if req.MCPConfig != "" {
		args = append(args, "--mcp-config", req.MCPConfig)
	}
	permission, err := permissionArgs(req.Permission)
	if err != nil {
		return nil, &runner.RunError{Kind: runner.ErrorInvalidRequest, Op: "claude session build", Err: err}
	}
	args = append(args, permission...)
	args = append(args, req.ExtraArgs...)

	return &sessionProtocol{
		command: runner.CommandSpec{
			Argv:        args,
			Dir:         req.WorkDir,
			Env:         cloneMap(req.Env),
			Interactive: true,
		},
	}, nil
}

func permissionArgs(mode runner.PermissionMode) ([]string, error) {
	switch mode {
	case "", runner.PermissionDefault:
		return nil, nil
	case runner.PermissionAcceptEdits:
		return []string{"--permission-mode", "acceptEdits"}, nil
	case runner.PermissionAuto:
		return []string{"--permission-mode", "auto"}, nil
	case runner.PermissionBypass:
		return []string{"--permission-mode", "bypassPermissions"}, nil
	case runner.PermissionManual:
		return []string{"--permission-mode", "manual"}, nil
	case runner.PermissionDontAsk:
		return []string{"--permission-mode", "dontAsk"}, nil
	case runner.PermissionPlan:
		return []string{"--permission-mode", "plan"}, nil
	default:
		return nil, fmt.Errorf("unsupported permission mode %q", mode)
	}
}

// sessionProtocol reuses the oneshot frame parser; protocolRun's per-turn
// accumulator (final text, usage, result subtype) is drained and reset at each
// `result` frame instead of at process exit.
type sessionProtocol struct {
	command runner.CommandSpec
	run     protocolRun
}

func (p *sessionProtocol) Command() runner.CommandSpec { return p.command }

func (p *sessionProtocol) SessionID() string { return p.run.sessionID }

func (p *sessionProtocol) EncodeTurn(input runner.TurnInput) ([]byte, error) {
	if strings.TrimSpace(input.Prompt) == "" {
		return nil, &runner.RunError{Kind: runner.ErrorInvalidRequest, Op: "claude session turn", Err: errors.New("prompt is required")}
	}
	frame := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": input.Prompt}},
		},
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return nil, &runner.RunError{Kind: runner.ErrorInvalidRequest, Op: "claude session turn", Err: err}
	}
	return append(payload, '\n'), nil
}

func (p *sessionProtocol) ParseLine(line []byte) ([]runner.Event, bool, error) {
	events, err := p.run.ParseLine(line)
	if err != nil {
		return nil, false, err
	}
	return events, p.run.resultSeen, nil
}

func (p *sessionProtocol) TurnResult(duration time.Duration) (runner.Result, error) {
	result, err := p.run.Finalize(runner.ExitStatus{ExitCode: 0}, "", duration)
	p.run.resultSeen = false
	p.run.resultIsError = false
	p.run.finalText = ""
	p.run.resultError = ""
	p.run.resultSubtype = ""
	p.run.usage = runner.Usage{}
	p.run.fallbackUsage = runner.Usage{}
	return result, err
}
