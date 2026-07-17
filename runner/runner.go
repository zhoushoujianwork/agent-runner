package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"
)

const (
	defaultMaxFrameBytes  = 16 << 20
	defaultMaxStderrBytes = 64 << 10
)

type Runner struct {
	Engine   Engine
	Executor Executor
}

// Run executes one prompt as a degenerate session: open, send a single turn,
// close. It validates the request, starts the backend process, and returns an
// asynchronous handle. Start failures are returned synchronously.
func (r *Runner) Run(ctx context.Context, req Request) (*RunHandle, error) {
	if ctx == nil {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "run", Err: errors.New("nil context")}
	}
	if r == nil || r.Engine == nil || r.Executor == nil {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "run", Err: errors.New("engine and executor are required")}
	}

	sessionReq := SessionRequest{
		WorkDir:            req.WorkDir,
		Model:              req.Model,
		AppendSystemPrompt: req.AppendSystemPrompt,
		ResumeSessionID:    req.SessionID,
		NewSessionID:       req.NewSessionID,
		Continue:           req.Continue,
		MaxTurns:           req.MaxTurns,
		AllowedTools:       req.AllowedTools,
		DisallowedTools:    req.DisallowedTools,
		MCPConfig:          req.MCPConfig,
		Permission:         req.Permission,
		Env:                req.Env,
		ExtraArgs:          req.ExtraArgs,
		ExtraDirs:          req.ExtraDirs,
		MaxFrameBytes:      req.MaxFrameBytes,
		MaxStderrBytes:     req.MaxStderrBytes,
		OnPermission:       req.OnPermission,
	}

	baseCtx, baseCancel := context.WithCancel(ctx)
	runCtx := baseCtx
	cancel := context.CancelFunc(baseCancel)
	if req.WallTimeout > 0 {
		var timeoutCancel context.CancelFunc
		runCtx, timeoutCancel = context.WithTimeout(baseCtx, req.WallTimeout)
		cancel = func() {
			timeoutCancel()
			baseCancel()
		}
	}

	session, err := r.openSession(runCtx, sessionReq, true)
	if err != nil {
		cancel()
		return nil, err
	}
	turn, err := session.Send(runCtx, TurnInput{Prompt: req.Prompt, IdleTimeout: req.IdleTimeout})
	if err != nil {
		_ = session.Close()
		cancel()
		return nil, err
	}
	// A one-shot run never sends a second turn, so close the input half right
	// away: agents that read stdin to EOF before answering would otherwise
	// deadlock against us waiting for their result (v0.2.0 static-stdin
	// behavior). Only a pending permission callback still needs stdin.
	if req.OnPermission == nil {
		_ = session.CloseInput()
	}

	handle := &RunHandle{events: turn.Events(), cancel: cancel, done: make(chan struct{})}
	go func() {
		result, runErr := turn.Wait()
		_ = session.Close()
		// Fold the real process exit into the one-shot result; a turn that
		// already failed keeps its own error and exit code.
		if exit, deadErr := session.Exit(); runErr == nil {
			result.ExitCode = exit.ExitCode
			result.Signal = exit.Signal
			if deadErr != nil {
				runErr = deadErr
				result.Success = false
			}
		}
		handle.complete(result, runErr)
		cancel()
	}()
	return handle, nil
}

type streamSource uint8

const (
	sourceStdout streamSource = iota
	sourceStderr
)

type streamItem struct {
	source streamSource
	line   []byte
	err    error
	done   bool
}

type waitResult struct {
	status ExitStatus
	err    error
}

func scanLines(reader io.Reader, source streamSource, maxBytes int, out chan<- streamItem) {
	scanner := bufio.NewScanner(reader)
	initial := 64 << 10
	if maxBytes < initial {
		initial = maxBytes
	}
	if initial < 1 {
		initial = 1
	}
	scanner.Buffer(make([]byte, initial), maxBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		out <- streamItem{source: source, line: line}
	}
	out <- streamItem{source: source, err: scanner.Err(), done: true}
}

type tailBuffer struct {
	max  int
	data []byte
	mu   sync.Mutex
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.max <= 0 {
		return n, nil
	}
	if len(p) >= b.max {
		b.data = append(b.data[:0], p[len(p)-b.max:]...)
		return n, nil
	}
	overflow := len(b.data) + len(p) - b.max
	if overflow > 0 {
		copy(b.data, b.data[overflow:])
		b.data = b.data[:len(b.data)-overflow]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(append([]byte(nil), b.data...))
}
