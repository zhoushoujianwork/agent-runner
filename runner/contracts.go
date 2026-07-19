package runner

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/zhoushoujianwork/agent-runner/termscreen"
)

// Engine translates a provider-neutral session request into one bidirectional
// protocol session. One-shot runs are the degenerate case: open a session,
// send a single turn, close. NewSession must return independent state so one
// Engine can be used concurrently.
type Engine interface {
	NewSession(SessionRequest) (SessionProtocol, error)
}

// SessionProtocol owns the wire protocol for exactly one agent process. Parse
// and encode methods are serialized by the runner, so implementations need no
// locking.
//
// The protocol is bidirectional: ParseLine may return bytes that must be
// written back to the process stdin (control responses), and the runner can
// inject interrupt and permission-response frames between stdout lines.
type SessionProtocol interface {
	// Command returns the long-lived process spec; Interactive must be true.
	Command() CommandSpec
	// EncodeTurn encodes one user turn as bytes written verbatim to stdin.
	EncodeTurn(TurnInput) ([]byte, error)
	// EncodeInterrupt encodes a frame that asks the agent to abort the
	// in-flight turn without exiting. Engines without interrupt support return
	// ErrBackendUnsupported; the runner then falls back to killing the process.
	EncodeInterrupt() ([]byte, error)
	// EncodePermissionResponse encodes the answer to a ControlRequest
	// previously surfaced by ParseLine.
	EncodePermissionResponse(id string, decision PermissionDecision) ([]byte, error)
	// ParseLine parses one stdout line into a Step.
	ParseLine(line []byte) (Step, error)
	// TurnResult drains the just-completed turn's aggregate and resets the
	// per-turn accumulator for the next turn.
	TurnResult(duration time.Duration) (Result, error)
	// SessionID is the most recent provider session id ("" until observed).
	SessionID() string
}

// Step is the outcome of parsing one stdout line.
type Step struct {
	// Events are provider-neutral events for the in-flight turn.
	Events []Event
	// Reply is written back to the process stdin verbatim when non-empty
	// (protocol-level acknowledgements the engine can answer on its own).
	Reply []byte
	// Control is a provider request that needs an asynchronous answer from the
	// caller (permission prompts). The runner resolves it via the session's
	// PermissionFunc and writes EncodePermissionResponse back to stdin.
	Control *ControlRequest
	// EndOfTurn reports that this line closed the in-flight turn; TurnResult
	// is then called before the next turn's lines arrive.
	EndOfTurn bool
}

// ControlRequest is a provider control-protocol request (e.g. Claude
// can_use_tool) that must be answered before the agent process continues.
type ControlRequest struct {
	ID       string
	ToolName string
	Input    json.RawMessage
	Raw      json.RawMessage
}

// Executor starts a shell-free CommandSpec in a concrete backend.
type Executor interface {
	Start(context.Context, CommandSpec) (Process, error)
}

// TermEngine is the optional capability implemented by engines whose CLI has
// an interactive TUI mode. NewTerm builds the interactive-TUI process spec (no
// --print, no wire protocol); the session fields carry the same semantics as
// SessionRequest so headless and TUI runs of the same conversation resume via
// the provider's own session mechanism. Engines that lack a TUI mode simply do
// not implement it, and OpenTerm returns ErrBackendUnsupported.
type TermEngine interface {
	NewTerm(TermRequest) (CommandSpec, error)
}

// PTYExecutor is the optional capability implemented by executors that can
// place a process inside a pseudo-terminal. StartPTY starts the spec attached
// to a PTY sized to size and returns a live PTYProcess. Executors without PTY
// support do not implement it, and OpenTerm returns ErrBackendUnsupported.
type PTYExecutor interface {
	StartPTY(ctx context.Context, spec CommandSpec, size TermSize) (PTYProcess, error)
}

// PTYProcess is one live process attached to a PTY. Output carries the merged
// raw terminal byte stream (stdout+stderr, VT escape sequences included); the
// runner never parses it. Reads on Output drain to EOF after the process
// exits.
type PTYProcess interface {
	Input() io.Writer
	Output() io.Reader
	Resize(TermSize) error
	Wait() (ExitStatus, error)
	Cancel() error
	PID() int
}

// StdinWriter is implemented by processes started from an Interactive
// CommandSpec. The writer stays open across turns; closing it asks the agent
// process to finish up and exit.
type StdinWriter interface {
	StdinWriter() io.WriteCloser
}

// Process is the minimum lifecycle surface shared by host, Docker and future
// sandbox executors.
type Process interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Wait() (ExitStatus, error)
	Cancel() error
	PID() int
}

// TermSemantics is the optional engine capability that interprets a TUI screen
// semantically — the "how to read this CLI's screen" knowledge (reply blocks,
// working/idle state, blocking menus) that would otherwise be re-implemented by
// every caller doing screen scraping. Engines without it leave TUI bytes
// uninterpreted; termmirror.New returns ErrBackendUnsupported.
type TermSemantics interface {
	// NewTermObserver binds a fresh observer to a mirrored screen. One observer
	// per process; it is not goroutine-safe (the caller serialises access, as
	// termmirror does).
	NewTermObserver(screen *termscreen.Screen) TermObserver
}

// TermObserver reads a mirrored TUI screen and reports its current semantics.
type TermObserver interface {
	// NewTurn resets per-turn state (extraction baseline, turn boundary). Call
	// it right after injecting a user turn into the terminal.
	NewTurn()
	// Observe samples the screen. now is the observation time; hadBytes
	// reports whether terminal output arrived since the previous call —
	// silence ticks (hadBytes=false) drive turn-boundary detection.
	Observe(now time.Time, hadBytes bool) TermObservation
}

// TermObservation is one semantic sample of a TUI screen.
type TermObservation struct {
	// Reply is the newest assistant reply as normalized speakable text.
	Reply string
	// ReplyChanged reports that Reply differs from the previous observation
	// (spinner redraws and unrelated repaints do not set it).
	ReplyChanged bool
	// TurnEnded reports that boundary detection judged the in-flight turn
	// finished (output settled, no spinner, idle prompt back, no blocking menu).
	TurnEnded bool
	// Prompt is the blocking interactive menu currently awaiting an answer
	// (tool permission, edit confirm, trust, upsell), nil when none. Answer it
	// by writing the chosen option's Key to the terminal input.
	Prompt *TermPrompt
}

// TermPrompt is a parsed blocking select-menu on the TUI screen.
type TermPrompt struct {
	Kind     string // "permission" | "editConfirm" | "upsell" | "trust" | "unknown"
	Question string
	Options  []TermPromptOption
	// Signature fingerprints the option set; a changed signature means a new
	// menu superseded the previous one.
	Signature string
}

// TermPromptOption is one selectable menu row; Key is the literal input to
// write (a digit today) to pick it, Default marks the highlighted row.
type TermPromptOption struct {
	Key     string
	Label   string
	Default bool
}
