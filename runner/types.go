package runner

import (
	"context"
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
	ExtraDirs          []ExtraDir        `json:"extra_dirs,omitempty"`
	WallTimeout        time.Duration     `json:"-"`
	IdleTimeout        time.Duration     `json:"-"`
	MaxFrameBytes      int               `json:"max_frame_bytes,omitempty"`
	MaxStderrBytes     int               `json:"max_stderr_bytes,omitempty"`
	// OnPermission answers provider permission prompts (tool approvals). Nil
	// denies every prompt; providers only emit prompts when it is set.
	OnPermission PermissionFunc `json:"-"`
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
	// ExtraDirs are context directories the executor exposes inside Dir before
	// the process starts (host: symlinks; future backends: mounts).
	ExtraDirs []ExtraDir
}

// ExtraDir exposes agent context from a source directory inside the process
// working directory, so the agent CLI's own discovery mechanism (.claude,
// .agent) picks it up. The executor owns placement: the host backend symlinks;
// future backends may mount.
//
// Default (Target empty) is discovery mode: Source is a context root (e.g. a
// project checkout). Its .claude/ and .agent/ convention directories are
// scanned and the entries of their content dirs (skills/, agents/, commands/)
// are linked one by one into the corresponding place under the working
// directory — Source/.claude/skills/foo appears as <cwd>/.claude/skills/foo.
// Entries already present locally win and are skipped; identical existing
// links are adopted without ownership; a Source without convention dirs is a
// silent no-op.
//
// Setting Target switches to exact mode: Source itself is linked at Target
// (relative to the working directory, no escaping), and an existing Target is
// an error.
type ExtraDir struct {
	// Source is the context root directory (discovery mode) or the directory
	// to link verbatim (exact mode).
	Source string `json:"source"`
	// Target, when set, selects exact mode and is the link location relative
	// to the working directory.
	Target string `json:"target,omitempty"`
	// Keep leaves the created links in place after the process exits. The
	// default removes every link this run created (never adopted ones), so
	// workspaces do not accumulate stale links to moved or deleted sources.
	Keep bool `json:"keep,omitempty"`
}

// SessionRequest opens one persistent agent process that accepts many turns.
// Turn prompts arrive via Session.Send, never through this request.
type SessionRequest struct {
	WorkDir            string            `json:"cwd,omitempty"`
	Model              string            `json:"model,omitempty"`
	AppendSystemPrompt string            `json:"append_system_prompt,omitempty"`
	ResumeSessionID    string            `json:"resume_session_id,omitempty"`
	NewSessionID       string            `json:"new_session_id,omitempty"`
	Continue           bool              `json:"continue,omitempty"`
	MaxTurns           int               `json:"max_turns,omitempty"`
	AllowedTools       []string          `json:"allowed_tools,omitempty"`
	DisallowedTools    []string          `json:"disallowed_tools,omitempty"`
	MCPConfig          string            `json:"mcp_config,omitempty"`
	Permission         PermissionMode    `json:"permission,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	ExtraArgs          []string          `json:"extra_args,omitempty"`
	ExtraDirs          []ExtraDir        `json:"extra_dirs,omitempty"`
	// TurnIdleTimeout is the default per-turn no-output timeout. It fires only
	// while a turn is in flight; an idle session between turns never trips it.
	TurnIdleTimeout time.Duration `json:"-"`
	// CloseGrace bounds Close's wait for a natural exit after stdin closes
	// before escalating to Process.Cancel. It also bounds the wait for a turn
	// interrupt to take effect before the process is killed. Zero uses a 3s
	// default.
	CloseGrace     time.Duration `json:"-"`
	MaxFrameBytes  int           `json:"max_frame_bytes,omitempty"`
	MaxStderrBytes int           `json:"max_stderr_bytes,omitempty"`
	// OnPermission answers provider permission prompts (tool approvals). Nil
	// denies every prompt; providers only emit prompts when it is set.
	OnPermission PermissionFunc `json:"-"`
}

// TurnInput is one user turn sent into a live session.
type TurnInput struct {
	Prompt string `json:"prompt"`
	// IdleTimeout overrides the session default for this turn; zero inherits.
	IdleTimeout time.Duration `json:"-"`
}

// PermissionRequest is one provider tool-approval prompt.
type PermissionRequest struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input,omitempty"`
	Raw      json.RawMessage `json:"raw,omitempty"`
}

// PermissionDecision answers a PermissionRequest. The zero value denies.
type PermissionDecision struct {
	Allow bool `json:"allow"`
	// UpdatedInput optionally rewrites the tool input on allow; nil keeps the
	// original input.
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	// Message is the reason reported to the agent on deny.
	Message string `json:"message,omitempty"`
}

// PermissionFunc resolves permission prompts for a session. It runs off the
// session's reader goroutine and may block on human input; the turn idle
// timer is paused while a prompt is pending. The context is the in-flight
// turn's Send context.
type PermissionFunc func(context.Context, PermissionRequest) (PermissionDecision, error)

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
