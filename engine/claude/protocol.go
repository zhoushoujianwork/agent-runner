package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhoushoujianwork/agent-runner/runner"
)

// sessionProtocol owns the stream-json wire protocol for one claude process.
// The runner serializes every call, so no locking is needed. The per-turn
// accumulator (final text, usage, result subtype) is drained and reset at each
// `result` frame by TurnResult.
type sessionProtocol struct {
	command runner.CommandSpec

	sessionID     string
	finalText     string
	usage         runner.Usage
	fallbackUsage runner.Usage
	resultSeen    bool
	resultIsError bool
	resultError   string
	resultSubtype string

	interruptSeq int
	// pendingInputs remembers the original tool input per can_use_tool request
	// so an allow decision without UpdatedInput echoes the input back, which
	// the CLI requires.
	pendingInputs map[string]json.RawMessage
}

func (p *sessionProtocol) Command() runner.CommandSpec { return p.command }

func (p *sessionProtocol) SessionID() string { return p.sessionID }

func (p *sessionProtocol) EncodeTurn(input runner.TurnInput) ([]byte, error) {
	if strings.TrimSpace(input.Prompt) == "" {
		return nil, &runner.RunError{Kind: runner.ErrorInvalidRequest, Op: "claude turn", Err: errors.New("prompt is required")}
	}
	frame := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": input.Prompt}},
		},
	}
	return marshalFrame(frame, "claude turn")
}

// EncodeInterrupt asks the CLI to abort the in-flight turn without exiting;
// the CLI answers with a control_response and closes the turn with a result
// frame.
func (p *sessionProtocol) EncodeInterrupt() ([]byte, error) {
	p.interruptSeq++
	frame := map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("agent-runner-%d", p.interruptSeq),
		"request":    map[string]any{"subtype": "interrupt"},
	}
	return marshalFrame(frame, "claude interrupt")
}

func (p *sessionProtocol) EncodePermissionResponse(id string, decision runner.PermissionDecision) ([]byte, error) {
	var inner map[string]any
	if decision.Allow {
		updated := decision.UpdatedInput
		if updated == nil {
			updated = p.pendingInputs[id]
		}
		if updated == nil {
			updated = json.RawMessage("{}")
		}
		inner = map[string]any{"behavior": "allow", "updatedInput": updated}
	} else {
		message := decision.Message
		if message == "" {
			message = "denied by caller"
		}
		inner = map[string]any{"behavior": "deny", "message": message}
	}
	delete(p.pendingInputs, id)
	frame := map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": id,
			"response":   inner,
		},
	}
	return marshalFrame(frame, "claude permission response")
}

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
	RequestID  string                     `json:"request_id"`
	Request    *controlRequestBody        `json:"request"`
}

type controlRequestBody struct {
	Subtype  string          `json:"subtype"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
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

func (p *sessionProtocol) ParseLine(line []byte) (runner.Step, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return runner.Step{}, nil
	}
	var frame envelope
	if err := json.Unmarshal(line, &frame); err != nil {
		return runner.Step{}, fmt.Errorf("decode claude stream-json: %w", err)
	}
	if frame.Type == "" {
		return runner.Step{}, errors.New("decode claude stream-json: missing type")
	}
	if frame.SessionID != "" {
		p.sessionID = frame.SessionID
	}
	raw := append(json.RawMessage(nil), line...)
	var step runner.Step

	switch frame.Type {
	case "system":
		step.Events = append(step.Events, runner.Event{Type: runner.EventInit, Raw: raw})

	case "assistant":
		if frame.Message == nil {
			step.Events = append(step.Events, runner.Event{Type: runner.EventRaw, Raw: raw})
			break
		}
		addUsage(&p.fallbackUsage, parseUsage(frame.Message.Usage))
		for _, block := range frame.Message.Content {
			step.Events = append(step.Events, contentEvents(block, raw)...)
		}
		if len(step.Events) == 0 {
			step.Events = append(step.Events, runner.Event{Type: runner.EventRaw, Raw: raw})
		}

	case "user":
		if frame.Message == nil {
			step.Events = append(step.Events, runner.Event{Type: runner.EventRaw, Raw: raw})
			break
		}
		for _, block := range frame.Message.Content {
			if block.Type == "tool_result" {
				step.Events = append(step.Events, runner.Event{
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
		if len(step.Events) == 0 {
			step.Events = append(step.Events, runner.Event{Type: runner.EventRaw, Raw: raw})
		}

	case "stream_event":
		step.Events = parseStreamEvent(frame.Event, raw)
		if len(step.Events) == 0 {
			step.Events = append(step.Events, runner.Event{Type: runner.EventRaw, Raw: raw})
		}

	case "control_request":
		return p.parseControlRequest(frame, raw)

	case "control_response", "control_cancel_request":
		// Protocol-level acknowledgements (e.g. the interrupt ack) carry no
		// caller-visible content; the interrupted turn still ends with its own
		// result frame.
		return runner.Step{}, nil

	case "result":
		p.resultSeen = true
		p.resultIsError = frame.IsError
		p.finalText = frame.Result
		p.resultError = frame.Error
		p.resultSubtype = frame.Subtype
		p.usage = parseUsage(frame.Usage)
		if frame.TotalCost != 0 {
			p.usage.CostUSD = frame.TotalCost
		} else if frame.CostUSD != 0 {
			p.usage.CostUSD = frame.CostUSD
		}
		p.usage.ModelUsage = parseModelUsage(frame.ModelUsage)
		usage := p.usage
		step.Events = append(step.Events, runner.Event{Type: runner.EventUsage, Usage: &usage, Raw: raw})

	default:
		step.Events = append(step.Events, runner.Event{Type: runner.EventRaw, Raw: raw})
	}

	for i := range step.Events {
		step.Events[i].SessionID = p.sessionID
	}
	step.EndOfTurn = p.resultSeen
	return step, nil
}

// parseControlRequest routes CLI-initiated control requests. can_use_tool
// becomes a ControlRequest for the runner's permission callback; anything else
// is answered inline with an error response so the CLI never hangs waiting.
func (p *sessionProtocol) parseControlRequest(frame envelope, raw json.RawMessage) (runner.Step, error) {
	if frame.Request != nil && frame.Request.Subtype == "can_use_tool" {
		if p.pendingInputs == nil {
			p.pendingInputs = make(map[string]json.RawMessage)
		}
		p.pendingInputs[frame.RequestID] = cloneRaw(frame.Request.Input)
		return runner.Step{Control: &runner.ControlRequest{
			ID:       frame.RequestID,
			ToolName: frame.Request.ToolName,
			Input:    cloneRaw(frame.Request.Input),
			Raw:      raw,
		}}, nil
	}
	subtype := ""
	if frame.Request != nil {
		subtype = frame.Request.Subtype
	}
	reply, err := marshalFrame(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "error",
			"request_id": frame.RequestID,
			"error":      fmt.Sprintf("unsupported control request %q", subtype),
		},
	}, "claude control error")
	if err != nil {
		return runner.Step{}, err
	}
	return runner.Step{Reply: reply}, nil
}

func (p *sessionProtocol) TurnResult(duration time.Duration) (runner.Result, error) {
	usage := p.usage
	if !p.resultSeen {
		usage = p.fallbackUsage
	}
	result := runner.Result{
		Success:    p.resultSeen && !p.resultIsError,
		Text:       p.finalText,
		SessionID:  p.sessionID,
		Subtype:    p.resultSubtype,
		DurationMS: duration.Milliseconds(),
		Usage:      usage,
	}
	var err error
	switch {
	case !p.resultSeen:
		err = &runner.RunError{Kind: runner.ErrorProtocol, Op: "claude", Err: errors.New("turn ended without result event")}
	case p.resultIsError:
		message := p.resultError
		if message == "" {
			message = p.finalText
		}
		err = &runner.RunError{Kind: runner.ErrorProcess, Op: "claude result", Err: errors.New(message)}
	}

	p.resultSeen = false
	p.resultIsError = false
	p.finalText = ""
	p.resultError = ""
	p.resultSubtype = ""
	p.usage = runner.Usage{}
	p.fallbackUsage = runner.Usage{}
	return result, err
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

func marshalFrame(frame map[string]any, op string) ([]byte, error) {
	payload, err := json.Marshal(frame)
	if err != nil {
		return nil, &runner.RunError{Kind: runner.ErrorInvalidRequest, Op: op, Err: err}
	}
	return append(payload, '\n'), nil
}
