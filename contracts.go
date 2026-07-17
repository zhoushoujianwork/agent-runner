package runner

import (
	"context"
	"io"
	"time"
)

// Engine translates a provider-neutral request into one protocol run.
// NewRun must return independent state so one Engine can be used concurrently.
type Engine interface {
	NewRun(Request) (ProtocolRun, error)
}

// ProtocolRun owns the parser and result accumulator for exactly one process.
type ProtocolRun interface {
	Command() CommandSpec
	ParseLine([]byte) ([]Event, error)
	Finalize(ExitStatus, string, time.Duration) (Result, error)
}

// SessionEngine is implemented by engines that support persistent sessions:
// one long-lived process accepting many turns over stdin.
type SessionEngine interface {
	NewSession(SessionRequest) (SessionProtocol, error)
}

// SessionProtocol owns the wire protocol for exactly one persistent process.
// It is confined to the session's reader goroutine, so implementations need
// no locking.
type SessionProtocol interface {
	// Command returns the long-lived process spec; Interactive must be true.
	Command() CommandSpec
	// EncodeTurn encodes one user turn as bytes written verbatim to stdin.
	EncodeTurn(TurnInput) ([]byte, error)
	// ParseLine parses one stdout line. endOfTurn reports that the line closed
	// the in-flight turn; TurnResult must then be called before the next turn's
	// lines arrive.
	ParseLine(line []byte) (events []Event, endOfTurn bool, err error)
	// TurnResult drains the just-completed turn's aggregate and resets the
	// per-turn accumulator for the next turn.
	TurnResult(duration time.Duration) (Result, error)
	// SessionID is the most recent provider session id ("" until observed).
	SessionID() string
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
