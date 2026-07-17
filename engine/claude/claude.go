package claude

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zhoushoujianwork/agent-runner/runner"
)

type Engine struct {
	Binary string
}

func New(binary string) *Engine {
	if binary == "" {
		binary = "claude"
	}
	return &Engine{Binary: binary}
}

// NewSession builds one persistent claude process in realtime streaming-input
// mode. `--print --input-format stream-json` keeps the process alive across
// turns — each Session.Send writes one stream-json user frame to stdin and the
// turn ends at that turn's `result` frame. One-shot runs use the same protocol
// and simply close stdin after the first turn.
func (e *Engine) NewSession(req runner.SessionRequest) (runner.SessionProtocol, error) {
	conversationModes := 0
	if req.ResumeSessionID != "" {
		conversationModes++
	}
	if req.NewSessionID != "" {
		conversationModes++
	}
	if req.Continue {
		conversationModes++
	}
	if conversationModes > 1 {
		return nil, &runner.RunError{Kind: runner.ErrorInvalidRequest, Op: "claude session build", Err: errors.New("resume_session_id, new_session_id, and continue are mutually exclusive")}
	}

	args := []string{
		e.Binary,
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
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
	} else if req.Continue {
		args = append(args, "--continue")
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
	if req.OnPermission != nil {
		args = append(args, "--permission-prompt-tool", "stdio")
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

func cloneMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
