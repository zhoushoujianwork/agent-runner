package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// PermissionMode controls the approval mode passed to the agent CLI.
type PermissionMode string

const (
	PermissionDefault     PermissionMode = "default"
	PermissionAcceptEdits PermissionMode = "accept-edits"
	PermissionAuto        PermissionMode = "auto"
	PermissionBypass      PermissionMode = "bypass"
	PermissionManual      PermissionMode = "manual"
	PermissionDontAsk     PermissionMode = "dont-ask"
	PermissionPlan        PermissionMode = "plan"
)

// Request describes one headless agent turn. Session ownership and persistence
// remain the caller's responsibility.
type Request struct {
	Prompt             string            `json:"prompt"`
	WorkDir            string            `json:"cwd,omitempty"`
	Model              string            `json:"model,omitempty"`
	AppendSystemPrompt string            `json:"append_system_prompt,omitempty"`
	SessionID          string            `json:"session_id,omitempty"`
	NewSessionID       string            `json:"new_session_id,omitempty"`
	Continue           bool              `json:"continue,omitempty"`
	MaxTurns           int               `json:"max_turns,omitempty"`
	AllowedTools       []string          `json:"allowed_tools,omitempty"`
	DisallowedTools    []string          `json:"disallowed_tools,omitempty"`
	MCPConfig          string            `json:"mcp_config,omitempty"`
	Permission         PermissionMode    `json:"permission,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	ExtraArgs          []string          `json:"extra_args,omitempty"`
	WallTimeout        time.Duration     `json:"-"`
	IdleTimeout        time.Duration     `json:"-"`
	MaxFrameBytes      int               `json:"max_frame_bytes,omitempty"`
	MaxStderrBytes     int               `json:"max_stderr_bytes,omitempty"`
}

// CommandSpec is a shell-free process specification produced by an Engine and
// consumed by an Executor.
type CommandSpec struct {
	Argv  []string
	Dir   string
	Env   map[string]string
	Stdin []byte
	// Interactive requests a writable stdin pipe instead of the static Stdin
	// bytes. The started Process must implement StdinWriter.
	Interactive bool
}

// SessionRequest opens one persistent agent process that accepts many turns.
// Turn prompts arrive via Session.Send, never through this request.
type SessionRequest struct {
	WorkDir            string            `json:"cwd,omitempty"`
	Model              string            `json:"model,omitempty"`
	AppendSystemPrompt string            `json:"append_system_prompt,omitempty"`
	ResumeSessionID    string            `json:"resume_session_id,omitempty"`
	NewSessionID       string            `json:"new_session_id,omitempty"`
	MaxTurns           int               `json:"max_turns,omitempty"`
	AllowedTools       []string          `json:"allowed_tools,omitempty"`
	DisallowedTools    []string          `json:"disallowed_tools,omitempty"`
	MCPConfig          string            `json:"mcp_config,omitempty"`
	Permission         PermissionMode    `json:"permission,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	ExtraArgs          []string          `json:"extra_args,omitempty"`
	// TurnIdleTimeout is the default per-turn no-output timeout. It fires only
	// while a turn is in flight; an idle session between turns never trips it.
	TurnIdleTimeout time.Duration `json:"-"`
	// CloseGrace bounds Close's wait for a natural exit after stdin closes
	// before escalating to Process.Cancel. Zero uses a 3s default.
	CloseGrace     time.Duration `json:"-"`
	MaxFrameBytes  int           `json:"max_frame_bytes,omitempty"`
	MaxStderrBytes int           `json:"max_stderr_bytes,omitempty"`
}

// TurnInput is one user turn sent into a live session.
type TurnInput struct {
	Prompt string `json:"prompt"`
	// IdleTimeout overrides the session default for this turn; zero inherits.
	IdleTimeout time.Duration `json:"-"`
}

type EventType string

const (
	EventInit       EventType = "init"
	EventText       EventType = "text"
	EventTextDelta  EventType = "text_delta"
	EventThinking   EventType = "thinking"
	EventToolUse    EventType = "tool_use"
	EventToolResult EventType = "tool_result"
	EventUsage      EventType = "usage"
	EventResult     EventType = "result"
	EventDiagnostic EventType = "diagnostic"
	EventRaw        EventType = "raw"
)

type ToolEvent struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
}

type Usage struct {
	InputTokens              int64              `json:"input_tokens,omitempty"`
	OutputTokens             int64              `json:"output_tokens,omitempty"`
	CacheCreationInputTokens int64              `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64              `json:"cache_read_input_tokens,omitempty"`
	CostUSD                  float64            `json:"cost_usd,omitempty"`
	ModelUsage               map[string]float64 `json:"model_usage,omitempty"`
}

// Result is the terminal aggregate returned by RunHandle.Wait and
// TurnHandle.Wait.
type Result struct {
	Success   bool   `json:"success"`
	Text      string `json:"text,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// Subtype is the provider's terminal frame subtype when it reports one
	// (Claude: "success", "error_max_turns", ...). Empty when no result frame
	// was seen.
	Subtype    string `json:"subtype,omitempty"`
	ExitCode   int    `json:"exit_code"`
	Signal     string `json:"signal,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Usage      Usage  `json:"usage,omitempty"`
}

// Event is a stable provider-neutral event. Raw preserves the original frame
// for replay and forward compatibility.
type Event struct {
	Type      EventType       `json:"type"`
	Time      time.Time       `json:"time"`
	SessionID string          `json:"session_id,omitempty"`
	Text      string          `json:"text,omitempty"`
	Tool      *ToolEvent      `json:"tool,omitempty"`
	Usage     *Usage          `json:"usage,omitempty"`
	Result    *Result         `json:"result,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

type ExitStatus struct {
	ExitCode int
	Signal   string
}

type ErrorKind string

const (
	ErrorInvalidRequest ErrorKind = "invalid_request"
	ErrorStart          ErrorKind = "start"
	ErrorProtocol       ErrorKind = "protocol"
	ErrorProcess        ErrorKind = "process"
	ErrorCancelled      ErrorKind = "cancelled"
	ErrorTimeout        ErrorKind = "timeout"
	ErrorIdleTimeout    ErrorKind = "idle_timeout"
	// ErrorBusy: Send was called while a previous turn is still in flight.
	ErrorBusy ErrorKind = "busy"
	// ErrorClosed: the session was closed (or the process retired) before or
	// during the operation.
	ErrorClosed ErrorKind = "closed"
)

// RunError is suitable for errors.As and for stable CLI error handling.
type RunError struct {
	Kind     ErrorKind
	Op       string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *RunError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := string(e.Kind)
	if e.Op != "" {
		msg = e.Op + ": " + msg
	}
	if e.ExitCode != 0 {
		msg += fmt.Sprintf(" (exit %d)", e.ExitCode)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *RunError) Unwrap() error { return e.Err }

var ErrBackendUnsupported = errors.New("runner: backend unsupported")
