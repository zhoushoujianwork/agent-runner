package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	defaultMaxFrameBytes  = 16 << 20
	defaultMaxStderrBytes = 64 << 10
)

type Runner struct {
	Engine   Engine
	Executor Executor
}

// Run validates the request, starts the backend process, and returns an
// asynchronous handle. Start failures are returned synchronously.
func (r *Runner) Run(ctx context.Context, req Request) (*RunHandle, error) {
	if ctx == nil {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "run", Err: errors.New("nil context")}
	}
	if r == nil || r.Engine == nil || r.Executor == nil {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "run", Err: errors.New("engine and executor are required")}
	}
	if req.MaxFrameBytes <= 0 {
		req.MaxFrameBytes = defaultMaxFrameBytes
	}
	if req.MaxStderrBytes <= 0 {
		req.MaxStderrBytes = defaultMaxStderrBytes
	}

	protocol, err := r.Engine.NewRun(req)
	if err != nil {
		return nil, err
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

	process, err := r.Executor.Start(runCtx, protocol.Command())
	if err != nil {
		cancel()
		return nil, &RunError{Kind: ErrorStart, Op: "executor start", Err: err}
	}

	eventsIn := make(chan Event, 64)
	eventsOut := make(chan Event)
	handle := &RunHandle{events: eventsOut, cancel: cancel, done: make(chan struct{})}
	go pumpEvents(eventsIn, eventsOut)
	go r.drive(runCtx, cancel, req, protocol, process, eventsIn, handle)
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

func (r *Runner) drive(
	ctx context.Context,
	cancel context.CancelFunc,
	req Request,
	protocol ProtocolRun,
	process Process,
	events chan<- Event,
	handle *RunHandle,
) {
	started := time.Now()
	defer cancel()
	defer close(events)

	items := make(chan streamItem, 128)
	go scanLines(process.Stdout(), sourceStdout, req.MaxFrameBytes, items)
	go scanLines(process.Stderr(), sourceStderr, req.MaxFrameBytes, items)

	waitCh := make(chan waitResult, 1)
	go func() {
		status, err := process.Wait()
		waitCh <- waitResult{status: status, err: err}
	}()

	stderrTail := newTailBuffer(req.MaxStderrBytes)
	stdoutDone, stderrDone, processDone := false, false, false
	status := ExitStatus{ExitCode: -1}
	var terminalErr error
	ctxDone := ctx.Done()

	var idleTimer *time.Timer
	var idle <-chan time.Time
	if req.IdleTimeout > 0 {
		idleTimer = time.NewTimer(req.IdleTimeout)
		idle = idleTimer.C
		defer idleTimer.Stop()
	}

	resetIdle := func() {
		if idleTimer == nil {
			return
		}
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(req.IdleTimeout)
	}

	emit := func(event Event) {
		if event.Time.IsZero() {
			event.Time = time.Now().UTC()
		}
		events <- event
	}

	for !stdoutDone || !stderrDone || !processDone {
		select {
		case item := <-items:
			if item.done {
				if item.source == sourceStdout {
					stdoutDone = true
				} else {
					stderrDone = true
				}
				if item.err != nil && !errors.Is(item.err, os.ErrClosed) && terminalErr == nil {
					terminalErr = &RunError{Kind: ErrorProtocol, Op: "read process stream", Err: item.err}
					_ = process.Cancel()
				}
				continue
			}
			resetIdle()
			if item.source == sourceStderr {
				line := redactSecrets(string(item.line))
				_, _ = stderrTail.Write(append([]byte(line), '\n'))
				emit(Event{Type: EventDiagnostic, Text: line})
				continue
			}
			parsed, err := protocol.ParseLine(item.line)
			if err != nil {
				if terminalErr == nil {
					terminalErr = &RunError{Kind: ErrorProtocol, Op: "parse stdout", Err: err}
					_ = process.Cancel()
				}
				continue
			}
			for _, event := range parsed {
				emit(event)
			}

		case waited := <-waitCh:
			processDone = true
			status = waited.status
			if waited.err != nil && terminalErr == nil {
				terminalErr = &RunError{Kind: ErrorProcess, Op: "wait process", Err: waited.err}
			}

		case <-idle:
			if terminalErr == nil {
				terminalErr = &RunError{
					Kind: ErrorIdleTimeout,
					Op:   "run",
					Err:  fmt.Errorf("no process output for %s", req.IdleTimeout),
				}
				_ = process.Cancel()
			}
			idle = nil

		case <-ctxDone:
			if terminalErr == nil {
				kind := ErrorCancelled
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					kind = ErrorTimeout
				}
				terminalErr = &RunError{Kind: kind, Op: "run", Err: ctx.Err()}
				_ = process.Cancel()
			}
			ctxDone = nil
		}
	}

	duration := time.Since(started)
	result, protocolErr := protocol.Finalize(status, stderrTail.String(), duration)
	if terminalErr == nil {
		terminalErr = protocolErr
	}
	if terminalErr != nil {
		result.Success = false
	}
	emit(Event{Type: EventResult, SessionID: result.SessionID, Result: &result})
	handle.complete(result, terminalErr)
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
