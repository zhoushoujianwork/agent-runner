package runner

import (
	"context"
	"encoding/json"
	"io"
	"time"
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
