package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	runner "github.com/zhoushoujianwork/agent-runner"
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

func (e *Engine) NewRun(req runner.Request) (runner.ProtocolRun, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, &runner.RunError{Kind: runner.ErrorInvalidRequest, Op: "claude build", Err: errors.New("prompt is required")}
	}
	conversationModes := 0
	if req.SessionID != "" {
		conversationModes++
	}
	if req.NewSessionID != "" {
		conversationModes++
	}
	if req.Continue {
		conversationModes++
	}
	if conversationModes > 1 {
		return nil, &runner.RunError{Kind: runner.ErrorInvalidRequest, Op: "claude build", Err: errors.New("session_id, new_session_id, and continue are mutually exclusive")}
	}

	args := []string{
		e.Binary,
		"--print",
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
	if req.SessionID != "" {
		args = append(args, "--resume", req.SessionID)
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
	switch req.Permission {
	case "", runner.PermissionDefault:
	case runner.PermissionAcceptEdits:
		args = append(args, "--permission-mode", "acceptEdits")
	case runner.PermissionAuto:
		args = append(args, "--permission-mode", "auto")
	case runner.PermissionBypass:
		args = append(args, "--permission-mode", "bypassPermissions")
	case runner.PermissionManual:
		args = append(args, "--permission-mode", "manual")
	case runner.PermissionDontAsk:
		args = append(args, "--permission-mode", "dontAsk")
	case runner.PermissionPlan:
		args = append(args, "--permission-mode", "plan")
	default:
		return nil, &runner.RunError{
			Kind: runner.ErrorInvalidRequest,
			Op:   "claude build",
			Err:  fmt.Errorf("unsupported permission mode %q", req.Permission),
		}
	}
	args = append(args, req.ExtraArgs...)

	stdin := append([]byte(req.Prompt), '\n')
	return &protocolRun{
		command: runner.CommandSpec{
			Argv:  args,
			Dir:   req.WorkDir,
			Env:   cloneMap(req.Env),
			Stdin: stdin,
		},
	}, nil
}

type protocolRun struct {
	command       runner.CommandSpec
	sessionID     string
	finalText     string
	usage         runner.Usage
	fallbackUsage runner.Usage
	resultSeen    bool
	resultIsError bool
	resultError   string
}

func (p *protocolRun) Command() runner.CommandSpec { return p.command }

type envelope struct {
	Type       string                     `json:"type"`
	Subtype    string                     `json:"subtype"`
	SessionID  string                     `json:"session_id"`
	Result     string                     `json:"result"`
	IsError    bool                       `json:"is_error"`
	Error      string                     `json:"error"`
	Usage      json.RawMessage            `json:"usage"`
	CostUSD    float64                    `json:"cost_usd"`
	TotalCost  float64                    `json:"total_cost_usd"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	Message    *message                   `json:"message"`
	Event      json.RawMessage            `json:"event"`
}

type message struct {
	Content []contentBlock  `json:"content"`
	Usage   json.RawMessage `json:"usage"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

func (p *protocolRun) ParseLine(line []byte) ([]runner.Event, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}
	var frame envelope
	if err := json.Unmarshal(line, &frame); err != nil {
		return nil, fmt.Errorf("decode claude stream-json: %w", err)
	}
	if frame.Type == "" {
		return nil, errors.New("decode claude stream-json: missing type")
	}
	if frame.SessionID != "" {
		p.sessionID = frame.SessionID
	}
	raw := append(json.RawMessage(nil), line...)
	var events []runner.Event

	switch frame.Type {
	case "system":
		events = append(events, runner.Event{Type: runner.EventInit, Raw: raw})

	case "assistant":
		if frame.Message == nil {
			events = append(events, runner.Event{Type: runner.EventRaw, Raw: raw})
			break
		}
		addUsage(&p.fallbackUsage, parseUsage(frame.Message.Usage))
		for _, block := range frame.Message.Content {
			events = append(events, contentEvents(block, raw)...)
		}
		if len(events) == 0 {
			events = append(events, runner.Event{Type: runner.EventRaw, Raw: raw})
		}

	case "user":
		if frame.Message == nil {
			events = append(events, runner.Event{Type: runner.EventRaw, Raw: raw})
			break
		}
		for _, block := range frame.Message.Content {
			if block.Type == "tool_result" {
				events = append(events, runner.Event{
					Type: runner.EventToolResult,
					Tool: &runner.ToolEvent{
						ToolUseID: block.ToolUseID,
						Content:   cloneRaw(block.Content),
						IsError:   block.IsError,
					},
					Raw: raw,
				})
			}
		}
		if len(events) == 0 {
			events = append(events, runner.Event{Type: runner.EventRaw, Raw: raw})
		}

	case "stream_event":
		events = append(events, parseStreamEvent(frame.Event, raw)...)
		if len(events) == 0 {
			events = append(events, runner.Event{Type: runner.EventRaw, Raw: raw})
		}

	case "result":
		p.resultSeen = true
		p.resultIsError = frame.IsError
		p.finalText = frame.Result
		p.resultError = frame.Error
		p.usage = parseUsage(frame.Usage)
		if frame.TotalCost != 0 {
			p.usage.CostUSD = frame.TotalCost
		} else if frame.CostUSD != 0 {
			p.usage.CostUSD = frame.CostUSD
		}
		p.usage.ModelUsage = parseModelUsage(frame.ModelUsage)
		usage := p.usage
		events = append(events, runner.Event{Type: runner.EventUsage, Usage: &usage, Raw: raw})

	default:
		events = append(events, runner.Event{Type: runner.EventRaw, Raw: raw})
	}

	for i := range events {
		events[i].SessionID = p.sessionID
	}
	return events, nil
}

func contentEvents(block contentBlock, raw json.RawMessage) []runner.Event {
	switch block.Type {
	case "text":
		return []runner.Event{{Type: runner.EventText, Text: block.Text, Raw: raw}}
	case "thinking":
		return []runner.Event{{Type: runner.EventThinking, Text: block.Thinking, Raw: raw}}
	case "tool_use":
		return []runner.Event{{
			Type: runner.EventToolUse,
			Tool: &runner.ToolEvent{ID: block.ID, Name: block.Name, Input: cloneRaw(block.Input)},
			Raw:  raw,
		}}
	case "tool_result":
		return []runner.Event{{
			Type: runner.EventToolResult,
			Tool: &runner.ToolEvent{ToolUseID: block.ToolUseID, Content: cloneRaw(block.Content), IsError: block.IsError},
			Raw:  raw,
		}}
	default:
		return nil
	}
}

func parseStreamEvent(data, raw json.RawMessage) []runner.Event {
	if len(data) == 0 {
		return nil
	}
	var event struct {
		Type         string       `json:"type"`
		Delta        streamDelta  `json:"delta"`
		ContentBlock contentBlock `json:"content_block"`
	}
	if json.Unmarshal(data, &event) != nil {
		return nil
	}
	switch event.Type {
	case "content_block_delta":
		switch event.Delta.Type {
		case "text_delta":
			return []runner.Event{{Type: runner.EventTextDelta, Text: event.Delta.Text, Raw: raw}}
		case "thinking_delta":
			return []runner.Event{{Type: runner.EventThinking, Text: event.Delta.Thinking, Raw: raw}}
		}
	case "content_block_start":
		return contentEvents(event.ContentBlock, raw)
	}
	return nil
}

type streamDelta struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}

func (p *protocolRun) Finalize(status runner.ExitStatus, stderr string, duration time.Duration) (runner.Result, error) {
	usage := p.usage
	if !p.resultSeen {
		usage = p.fallbackUsage
	}
	result := runner.Result{
		Success:    status.ExitCode == 0 && p.resultSeen && !p.resultIsError,
		Text:       p.finalText,
		SessionID:  p.sessionID,
		ExitCode:   status.ExitCode,
		Signal:     status.Signal,
		DurationMS: duration.Milliseconds(),
		Usage:      usage,
	}
	if status.ExitCode != 0 {
		return result, &runner.RunError{
			Kind:     runner.ErrorProcess,
			Op:       "claude",
			ExitCode: status.ExitCode,
			Stderr:   stderr,
			Err:      errors.New(strings.TrimSpace(stderr)),
		}
	}
	if !p.resultSeen {
		return result, &runner.RunError{Kind: runner.ErrorProtocol, Op: "claude", Err: errors.New("stream ended without result event")}
	}
	if p.resultIsError {
		message := p.resultError
		if message == "" {
			message = p.finalText
		}
		return result, &runner.RunError{Kind: runner.ErrorProcess, Op: "claude result", Err: errors.New(message)}
	}
	return result, nil
}

func parseUsage(raw json.RawMessage) runner.Usage {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return runner.Usage{}
	}
	var value struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	}
	_ = json.Unmarshal(raw, &value)
	return runner.Usage{
		InputTokens:              value.InputTokens,
		OutputTokens:             value.OutputTokens,
		CacheCreationInputTokens: value.CacheCreationInputTokens,
		CacheReadInputTokens:     value.CacheReadInputTokens,
	}
}

func parseModelUsage(raw map[string]json.RawMessage) map[string]float64 {
	if len(raw) == 0 {
		return nil
	}
	result := make(map[string]float64, len(raw))
	for model, data := range raw {
		var value struct {
			CostUSD      float64 `json:"costUSD"`
			CostUSDSnake float64 `json:"cost_usd"`
		}
		if json.Unmarshal(data, &value) == nil {
			if value.CostUSD != 0 {
				result[model] = value.CostUSD
			} else {
				result[model] = value.CostUSDSnake
			}
		}
	}
	return result
}

func addUsage(target *runner.Usage, value runner.Usage) {
	target.InputTokens += value.InputTokens
	target.OutputTokens += value.OutputTokens
	target.CacheCreationInputTokens += value.CacheCreationInputTokens
	target.CacheReadInputTokens += value.CacheReadInputTokens
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
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
