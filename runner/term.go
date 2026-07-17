package runner

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// OpenTerm starts one interactive TUI process inside a pseudo-terminal and
// returns a Term for raw byte-stream I/O. Both the Engine and the Executor must
// support the optional term capabilities (TermEngine / PTYExecutor); either
// missing yields ErrBackendUnsupported. The returned Term is a zero-parse,
// zero-copy conduit: Input/Output pass bytes straight through and the runner
// never interprets them. ExtraDirs are prepared and cleaned up with the same
// lifecycle as headless sessions.
func (r *Runner) OpenTerm(ctx context.Context, req TermRequest) (*Term, error) {
	if ctx == nil {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "open term", Err: errors.New("nil context")}
	}
	if r == nil || r.Engine == nil || r.Executor == nil {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "open term", Err: errors.New("engine and executor are required")}
	}
	termEngine, ok := r.Engine.(TermEngine)
	if !ok {
		return nil, ErrBackendUnsupported
	}
	ptyExecutor, ok := r.Executor.(PTYExecutor)
	if !ok {
		return nil, ErrBackendUnsupported
	}
	if req.CloseGrace <= 0 {
		req.CloseGrace = defaultCloseGrace
	}

	spec, err := termEngine.NewTerm(req)
	if err != nil {
		return nil, err
	}

	termCtx, cancel := context.WithCancel(ctx)
	process, err := ptyExecutor.StartPTY(termCtx, spec, req.Size)
	if err != nil {
		cancel()
		if errors.Is(err, ErrBackendUnsupported) {
			return nil, err
		}
		return nil, &RunError{Kind: ErrorStart, Op: "executor start pty", Err: err}
	}

	t := &Term{
		process: process,
		cancel:  cancel,
		grace:   req.CloseGrace,
		deadCh:  make(chan struct{}),
	}
	go t.reap()
	return t, nil
}

// Term is one live interactive TUI process attached to a PTY. It owns only the
// process lifecycle: there is no protocol layer, no turn accounting and no
// internal pump. VT emulation, screen capture and approval detection are the
// caller's concern (they consume Output).
type Term struct {
	process PTYProcess
	cancel  context.CancelFunc
	grace   time.Duration

	deadCh    chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	exit    ExitStatus
	waitErr error
	dead    bool
}

// Input is the write half of the terminal: keyboard bytes written here reach
// the process stdin verbatim (e.g. an xterm.js onData feed).
func (t *Term) Input() io.Writer { return t.process.Input() }

// Output is the merged raw terminal byte stream (stdout+stderr with VT escape
// sequences). It drains to EOF after the process exits.
func (t *Term) Output() io.Reader { return t.process.Output() }

// Resize applies a new terminal geometry.
func (t *Term) Resize(size TermSize) error { return t.process.Resize(size) }

// PID is the process id of the backing process (0 when unavailable).
func (t *Term) PID() int { return t.process.PID() }

// Dead is closed once the process has exited. Output may still hold buffered
// bytes to drain to EOF after Dead fires.
func (t *Term) Dead() <-chan struct{} { return t.deadCh }

// Exit returns the process exit status and terminal error; valid once Dead is
// closed.
func (t *Term) Exit() (ExitStatus, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.exit, t.waitErr
}

// Close terminates the process (SIGTERM, then SIGKILL after CloseGrace) and is
// idempotent. It does not wait for Output to be drained; use Dead for that.
func (t *Term) Close() error {
	var err error
	t.closeOnce.Do(func() {
		err = t.process.Cancel()
		t.cancel()
	})
	return err
}

func (t *Term) reap() {
	status, waitErr := t.process.Wait()
	t.mu.Lock()
	t.exit = status
	t.waitErr = waitErr
	t.dead = true
	t.mu.Unlock()
	t.cancel()
	close(t.deadCh)
}
