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

// Executor starts a shell-free CommandSpec in a concrete backend.
type Executor interface {
	Start(context.Context, CommandSpec) (Process, error)
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
