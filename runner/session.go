package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	defaultCloseGrace       = 3 * time.Second
	maxPendingSessionEvents = 64
)

// OpenSession starts one persistent agent process that accepts many turns via
// Session.Send. The Executor must produce processes with a writable stdin
// (StdinWriter). Cancelling ctx kills the process; per-turn deadlines belong
// to Send.
func (r *Runner) OpenSession(ctx context.Context, req SessionRequest) (*Session, error) {
	return r.openSession(ctx, req, false)
}

// openSession is the shared constructor. killOnFail marks single-turn
// sessions (Runner.Run): the process is retired immediately when its only
// turn fails, instead of attempting a graceful interrupt.
func (r *Runner) openSession(ctx context.Context, req SessionRequest, killOnFail bool) (*Session, error) {
	if ctx == nil {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "open session", Err: errors.New("nil context")}
	}
	if r == nil || r.Engine == nil || r.Executor == nil {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "open session", Err: errors.New("engine and executor are required")}
	}
	if req.MaxFrameBytes <= 0 {
		req.MaxFrameBytes = defaultMaxFrameBytes
	}
	if req.MaxStderrBytes <= 0 {
		req.MaxStderrBytes = defaultMaxStderrBytes
	}
	if req.CloseGrace <= 0 {
		req.CloseGrace = defaultCloseGrace
	}

	protocol, err := r.Engine.NewSession(req)
	if err != nil {
		return nil, err
	}
	spec := protocol.Command()
	if !spec.Interactive {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "open session", Err: errors.New("session command spec must be interactive")}
	}

	sessCtx, cancel := context.WithCancel(ctx)
	process, err := r.Executor.Start(sessCtx, spec)
	if err != nil {
		cancel()
		return nil, &RunError{Kind: ErrorStart, Op: "executor start", Err: err}
	}
	writer, ok := process.(StdinWriter)
	if !ok {
		cancel()
		_ = process.Cancel()
		return nil, &RunError{Kind: ErrorStart, Op: "open session", Err: fmt.Errorf("executor process %T does not expose stdin", process)}
	}

	s := &Session{
		req:        req,
		protocol:   protocol,
		process:    process,
		stdin:      writer.StdinWriter(),
		cancel:     cancel,
		killOnFail: killOnFail,
		stderrTail: newTailBuffer(req.MaxStderrBytes),
		readyCh:    make(chan struct{}),
		deadCh:     make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

// Session is one live persistent agent process. Turns are strictly serial:
// Send fails with ErrorBusy while a previous turn is in flight.
type Session struct {
	req        SessionRequest
	protocol   SessionProtocol
	process    Process
	stdin      io.WriteCloser
	cancel     context.CancelFunc
	killOnFail bool

	stderrTail *tailBuffer

	readyCh   chan struct{}
	readyOnce sync.Once
	deadCh    chan struct{}
	closeOnce sync.Once

	// writeMu serializes stdin writes: turn payloads (Send), protocol replies
	// (readLoop), interrupt frames and permission responses (watcher and
	// permission goroutines).
	writeMu sync.Mutex

	mu          sync.Mutex
	turn        *TurnHandle
	zombies     []*TurnHandle // failed turns whose event channel the reader still owns
	pending     []Event       // events parsed while no turn was mounted (bounded)
	closed      bool
	inputClosed bool // stdin closed via CloseInput; the in-flight turn may still finish
	dying       bool // a failed turn killed the process; refuse new turns until dead
	dead        bool
	exit        ExitStatus
	deadErr     error
}

// Ready is closed when the process emits its first init event (skills, tools
// and MCP servers loaded). Useful for prewarming.
func (s *Session) Ready() <-chan struct{} { return s.readyCh }

// Dead is closed when the process has exited and its streams are drained.
func (s *Session) Dead() <-chan struct{} { return s.deadCh }

// Alive reports whether the process has not been observed dead yet.
func (s *Session) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.dead
}

// PID is the process id of the backing process (0 when unavailable).
func (s *Session) PID() int { return s.process.PID() }

// SessionID is the most recent provider session id ("" until observed).
// It is stable to read between turns; during a turn prefer the turn result.
func (s *Session) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.protocol.SessionID()
}

// StderrTail returns the redacted tail of the process stderr for diagnostics.
func (s *Session) StderrTail() string { return s.stderrTail.String() }

// Exit returns the process exit status and terminal error once Dead is closed.
func (s *Session) Exit() (ExitStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exit, s.deadErr
}

// Send writes one user turn into the live process and returns its handle.
// Turns are serial: a Send while a turn is in flight fails with ErrorBusy.
// Cancelling ctx or hitting the turn idle timeout interrupts the turn; the
// session survives when the agent honours the interrupt within CloseGrace,
// and is killed otherwise.
func (s *Session) Send(ctx context.Context, input TurnInput) (*TurnHandle, error) {
	if ctx == nil {
		return nil, &RunError{Kind: ErrorInvalidRequest, Op: "session send", Err: errors.New("nil context")}
	}
	payload, err := s.protocolEncode(input)
	if err != nil {
		return nil, err
	}

	idleTimeout := input.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = s.req.TurnIdleTimeout
	}

	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil, &RunError{Kind: ErrorClosed, Op: "session send", Err: errors.New("session is closed")}
	case s.inputClosed:
		s.mu.Unlock()
		return nil, &RunError{Kind: ErrorClosed, Op: "session send", Err: errors.New("session input is closed")}
	case s.dead:
		exit, deadErr := s.exit, s.deadErr
		s.mu.Unlock()
		return nil, &RunError{
			Kind:     ErrorProcess,
			Op:       "session send",
			ExitCode: exit.ExitCode,
			Stderr:   s.stderrTail.String(),
			Err:      fmt.Errorf("session process already exited: %w", errOrExit(deadErr, exit)),
		}
	case s.dying:
		s.mu.Unlock()
		return nil, &RunError{Kind: ErrorClosed, Op: "session send", Err: errors.New("session is being retired after a failed turn")}
	case s.turn != nil:
		s.mu.Unlock()
		return nil, &RunError{Kind: ErrorBusy, Op: "session send", Err: errors.New("a previous turn is still in flight")}
	}
	turn := newTurnHandle(ctx)
	// Flush events parsed while no turn was mounted (process init, late frames)
	// ahead of this turn's live events. Safe: the reader cannot touch turn's
	// channel until s.turn is published below.
	for _, event := range s.pending {
		turn.eventsIn <- event
	}
	s.pending = nil
	s.turn = turn
	s.mu.Unlock()

	if err := s.writeStdin(payload); err != nil {
		failure := &RunError{
			Kind:   ErrorProcess,
			Op:     "session send",
			Stderr: s.stderrTail.String(),
			Err:    fmt.Errorf("write turn to stdin: %w", err),
		}
		s.failTurn(turn, failure, false)
		return nil, failure
	}

	if idleTimeout > 0 {
		turn.setIdleTimer(idleTimeout, func() {
			s.interruptTurn(turn, &RunError{
				Kind: ErrorIdleTimeout,
				Op:   "session turn",
				Err:  fmt.Errorf("no process output for %s", idleTimeout),
			})
		})
	}
	turn.setCtxWatch(context.AfterFunc(ctx, func() {
		kind := ErrorCancelled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			kind = ErrorTimeout
		}
		s.interruptTurn(turn, &RunError{Kind: kind, Op: "session turn", Err: ctx.Err()})
	}))
	return turn, nil
}

func (s *Session) protocolEncode(input TurnInput) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.protocol.EncodeTurn(input)
}

// writeStdin serializes writes into the live process stdin.
func (s *Session) writeStdin(payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.stdin.Write(payload)
	return err
}

// CloseInput closes the process stdin without retiring the session: the
// in-flight turn keeps streaming and completes normally, but no further turns
// can be sent. One-shot runs use it right after writing their only turn so
// agents that read stdin to EOF before answering are not deadlocked; for the
// real CLI it signals "no more turns", letting the process exit after the
// current turn. Idempotent.
func (s *Session) CloseInput() error {
	s.mu.Lock()
	if s.inputClosed {
		s.mu.Unlock()
		return nil
	}
	s.inputClosed = true
	s.mu.Unlock()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.stdin.Close()
}

func (s *Session) inputIsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inputClosed
}

// Close retires the session: close stdin so the agent process can finish
// naturally, escalate to Cancel after CloseGrace, and wait for the reader to
// drain. Idempotent and safe to call concurrently with an in-flight turn —
// that turn completes with ErrorClosed unless it finishes first.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		_ = s.stdin.Close()
		select {
		case <-s.deadCh:
		case <-time.After(s.req.CloseGrace):
			_ = s.process.Cancel()
		}
		<-s.deadCh
		s.cancel()
	})
	return nil
}

// interruptTurn asks the agent to abort the in-flight turn without losing the
// process: it marks the turn with its failure cause, writes the protocol's
// interrupt frame, and escalates to killing the process if the turn does not
// end within CloseGrace. Sessions opened for a single turn (killOnFail) and
// engines without interrupt support skip straight to the kill.
func (s *Session) interruptTurn(turn *TurnHandle, cause error) {
	if s.killOnFail {
		s.failTurn(turn, cause, true)
		return
	}
	s.mu.Lock()
	if s.turn != turn || s.dead || s.dying || turn.interruptCause != nil {
		s.mu.Unlock()
		return
	}
	turn.interruptCause = cause
	payload, err := s.protocol.EncodeInterrupt()
	s.mu.Unlock()

	if err != nil || len(payload) == 0 {
		s.failTurn(turn, cause, true)
		return
	}
	if err := s.writeStdin(payload); err != nil {
		s.failTurn(turn, cause, true)
		return
	}
	turn.setEscalation(time.AfterFunc(s.req.CloseGrace, func() {
		s.failTurn(turn, cause, true)
	}))
}

// failTurn detaches the in-flight turn (if it is still the current one),
// completes it with cause, and optionally kills the process. The turn's event
// channel is closed later by the reader on process death — every fail path
// either kills the process or follows its death, so the reader always exits.
func (s *Session) failTurn(turn *TurnHandle, cause error, kill bool) {
	s.mu.Lock()
	if s.turn != turn {
		s.mu.Unlock()
		return
	}
	s.turn = nil
	s.zombies = append(s.zombies, turn)
	if kill {
		s.dying = true
	}
	s.mu.Unlock()
	turn.stopWatchers()
	turn.complete(Result{Success: false, ExitCode: -1}, cause)
	if kill {
		_ = s.process.Cancel()
	}
}

func (s *Session) readLoop() {
	items := make(chan streamItem, 128)
	go scanLines(s.process.Stdout(), sourceStdout, s.req.MaxFrameBytes, items)
	go scanLines(s.process.Stderr(), sourceStderr, s.req.MaxFrameBytes, items)

	waitCh := make(chan waitResult, 1)
	go func() {
		status, err := s.process.Wait()
		waitCh <- waitResult{status: status, err: err}
	}()

	stdoutDone, stderrDone, processDone := false, false, false
	status := ExitStatus{ExitCode: -1}
	var terminalErr error

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
					terminalErr = &RunError{Kind: ErrorProtocol, Op: "read session stream", Err: item.err}
					_ = s.process.Cancel()
				}
				continue
			}
			if item.source == sourceStderr {
				line := redactSecrets(string(item.line))
				_, _ = s.stderrTail.Write(append([]byte(line), '\n'))
				s.dispatch([]Event{{Type: EventDiagnostic, Text: line}})
				continue
			}
			step, err := s.parseLine(item.line)
			if err != nil {
				if terminalErr == nil {
					terminalErr = &RunError{Kind: ErrorProtocol, Op: "parse session stdout", Err: err}
					_ = s.process.Cancel()
				}
				continue
			}
			if len(step.Reply) > 0 {
				if err := s.writeStdin(step.Reply); err != nil && terminalErr == nil && s.Alive() && !s.inputIsClosed() {
					terminalErr = &RunError{Kind: ErrorProtocol, Op: "write protocol reply", Err: err}
					_ = s.process.Cancel()
				}
			}
			if step.Control != nil {
				go s.answerControl(step.Control)
			}
			s.dispatch(step.Events)
			if step.EndOfTurn {
				s.completeTurn()
			}

		case waited := <-waitCh:
			processDone = true
			status = waited.status
			if waited.err != nil && terminalErr == nil {
				terminalErr = &RunError{Kind: ErrorProcess, Op: "wait session process", Err: waited.err}
			}
		}
	}
	s.finalizeDeath(status, terminalErr)
}

func (s *Session) parseLine(line []byte) (Step, error) {
	s.mu.Lock()
	step, err := s.protocol.ParseLine(line)
	s.mu.Unlock()
	for _, event := range step.Events {
		if event.Type == EventInit {
			s.readyOnce.Do(func() { close(s.readyCh) })
			break
		}
	}
	return step, err
}

// answerControl resolves one provider permission prompt off the reader
// goroutine: the callback may block on human input, so the turn idle timer is
// paused while the prompt is pending. A nil OnPermission denies.
func (s *Session) answerControl(ctrl *ControlRequest) {
	s.mu.Lock()
	turn := s.turn
	s.mu.Unlock()
	if turn != nil {
		turn.pauseIdle()
		defer turn.resumeIdle()
	}
	ctx := context.Background()
	if turn != nil && turn.ctx != nil {
		ctx = turn.ctx
	}

	decision := PermissionDecision{Message: "no permission handler configured"}
	if s.req.OnPermission != nil {
		var err error
		decision, err = s.req.OnPermission(ctx, PermissionRequest{
			ToolName: ctrl.ToolName,
			Input:    ctrl.Input,
			Raw:      ctrl.Raw,
		})
		if err != nil {
			decision = PermissionDecision{Message: err.Error()}
		}
	}

	s.mu.Lock()
	payload, err := s.protocol.EncodePermissionResponse(ctrl.ID, decision)
	s.mu.Unlock()
	if err == nil {
		err = s.writeStdin(payload)
	}
	if err != nil && s.Alive() {
		failure := &RunError{Kind: ErrorProtocol, Op: "session permission response", Err: err}
		if turn != nil {
			s.failTurn(turn, failure, true)
		} else {
			_ = s.process.Cancel()
		}
	}
}

// dispatch delivers events to the in-flight turn, or buffers a bounded number
// of them for the next turn when none is mounted.
func (s *Session) dispatch(events []Event) {
	if len(events) == 0 {
		return
	}
	now := time.Now().UTC()
	for i := range events {
		if events[i].Time.IsZero() {
			events[i].Time = now
		}
	}
	s.mu.Lock()
	turn := s.turn
	if turn == nil {
		room := maxPendingSessionEvents - len(s.pending)
		if room > 0 {
			if len(events) > room {
				events = events[:room]
			}
			s.pending = append(s.pending, events...)
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	turn.resetIdle()
	for _, event := range events {
		turn.eventsIn <- event
	}
}

func (s *Session) completeTurn() {
	s.mu.Lock()
	turn := s.turn
	s.turn = nil
	var result Result
	var err error
	if turn != nil {
		result, err = s.protocol.TurnResult(time.Since(turn.started))
		if turn.interruptCause != nil {
			// The turn ended because we interrupted it; the cause outranks the
			// provider's own error result.
			err = turn.interruptCause
			result.Success = false
		}
	}
	s.mu.Unlock()
	if turn == nil {
		return
	}
	turn.stopWatchers()
	turn.eventsIn <- Event{Type: EventResult, SessionID: result.SessionID, Result: &result}
	close(turn.eventsIn)
	turn.complete(result, err)
}

// finalizeDeath records the exit, completes a still-mounted turn with a
// process-death error, and closes every outstanding event channel. Runs once,
// at reader exit — the single owner of turn event channels.
func (s *Session) finalizeDeath(status ExitStatus, terminalErr error) {
	s.mu.Lock()
	turn := s.turn
	s.turn = nil
	zombies := s.zombies
	s.zombies = nil
	s.dead = true
	s.exit = status
	closed := s.closed
	sessionID := s.protocol.SessionID()
	var interruptCause error
	if turn != nil {
		interruptCause = turn.interruptCause
	}
	s.mu.Unlock()

	if turn != nil {
		turn.stopWatchers()
		err := terminalErr
		if err == nil {
			switch {
			case interruptCause != nil:
				err = interruptCause
			case closed:
				err = &RunError{Kind: ErrorClosed, Op: "session turn", Err: errors.New("session closed before the turn completed")}
			default:
				err = &RunError{
					Kind:     ErrorProcess,
					Op:       "session turn",
					ExitCode: status.ExitCode,
					Stderr:   s.stderrTail.String(),
					Err:      fmt.Errorf("session process exited before completing the turn: %s", exitString(status)),
				}
			}
		}
		result := Result{
			Success:   false,
			SessionID: sessionID,
			ExitCode:  status.ExitCode,
			Signal:    status.Signal,
		}
		turn.eventsIn <- Event{Type: EventResult, Time: time.Now().UTC(), SessionID: sessionID, Result: &result}
		turn.complete(result, err)
		close(turn.eventsIn)
	}
	for _, zombie := range zombies {
		close(zombie.eventsIn)
	}

	s.mu.Lock()
	if terminalErr == nil {
		terminalErr = s.deathError(status, closed)
	}
	s.deadErr = terminalErr
	s.mu.Unlock()
	close(s.deadCh)
	s.cancel()
}

// deathError classifies an unprompted process exit. A clean exit after Close
// is not an error.
func (s *Session) deathError(status ExitStatus, closed bool) error {
	if closed || status.ExitCode == 0 {
		return nil
	}
	return &RunError{
		Kind:     ErrorProcess,
		Op:       "session",
		ExitCode: status.ExitCode,
		Stderr:   s.stderrTail.String(),
		Err:      fmt.Errorf("session process exited: %s", exitString(status)),
	}
}

func errOrExit(err error, status ExitStatus) error {
	if err != nil {
		return err
	}
	return errors.New(exitString(status))
}

func exitString(status ExitStatus) string {
	if status.Signal != "" {
		return fmt.Sprintf("signal %s (exit %d)", status.Signal, status.ExitCode)
	}
	return fmt.Sprintf("exit %d", status.ExitCode)
}

// TurnHandle exposes one turn's streaming events independently from its
// terminal completion, mirroring RunHandle.
type TurnHandle struct {
	events   <-chan Event
	eventsIn chan Event
	done     chan struct{}
	started  time.Time
	ctx      context.Context // the Send context; passed to permission callbacks

	// interruptCause is the failure cause recorded when the runner interrupts
	// this turn; guarded by the owning session's mu.
	interruptCause error

	watchMu         sync.Mutex
	idleTimer       *time.Timer
	idleTimeout     time.Duration
	escalation      *time.Timer
	stopCtxWatch    func() bool
	watchersStopped bool

	mu           sync.RWMutex
	completeOnce sync.Once
	result       Result
	err          error
}

func newTurnHandle(ctx context.Context) *TurnHandle {
	eventsIn := make(chan Event, 64+maxPendingSessionEvents)
	eventsOut := make(chan Event)
	turn := &TurnHandle{
		events:   eventsOut,
		eventsIn: eventsIn,
		done:     make(chan struct{}),
		started:  time.Now(),
		ctx:      ctx,
	}
	go pumpEvents(eventsIn, eventsOut)
	return turn
}

// Events streams this turn's events. The channel closes shortly after the
// turn completes (immediately on a normal or interrupted turn end; after the
// process dies when the turn failure retired the whole session).
func (t *TurnHandle) Events() <-chan Event { return t.events }

// Wait blocks until the turn completes and returns its terminal aggregate.
// Consuming Events is optional; an internal queue prevents backpressure.
func (t *TurnHandle) Wait() (Result, error) {
	if t == nil {
		return Result{}, &RunError{Kind: ErrorInvalidRequest, Op: "turn wait"}
	}
	<-t.done
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.result, t.err
}

func (t *TurnHandle) complete(result Result, err error) {
	t.completeOnce.Do(func() {
		if result.DurationMS == 0 {
			result.DurationMS = time.Since(t.started).Milliseconds()
		}
		t.mu.Lock()
		t.result = result
		t.err = err
		t.mu.Unlock()
		close(t.done)
	})
}

func (t *TurnHandle) setIdleTimer(timeout time.Duration, onFire func()) {
	t.watchMu.Lock()
	if t.watchersStopped {
		t.watchMu.Unlock()
		return
	}
	t.idleTimeout = timeout
	t.idleTimer = time.AfterFunc(timeout, onFire)
	t.watchMu.Unlock()
}

// setCtxWatch registers the ctx-cancel watcher; a turn that already completed
// (stopWatchers ran before Send finished wiring) releases it immediately.
func (t *TurnHandle) setCtxWatch(stop func() bool) {
	t.watchMu.Lock()
	if t.watchersStopped {
		t.watchMu.Unlock()
		stop()
		return
	}
	t.stopCtxWatch = stop
	t.watchMu.Unlock()
}

// setEscalation arms the interrupt-escalation timer; a turn that completed in
// the meantime stops it immediately.
func (t *TurnHandle) setEscalation(timer *time.Timer) {
	t.watchMu.Lock()
	if t.watchersStopped {
		t.watchMu.Unlock()
		timer.Stop()
		return
	}
	t.escalation = timer
	t.watchMu.Unlock()
}

func (t *TurnHandle) resetIdle() {
	t.watchMu.Lock()
	if t.idleTimer != nil {
		t.idleTimer.Reset(t.idleTimeout)
	}
	t.watchMu.Unlock()
}

// pauseIdle suspends the idle timer while a permission prompt is pending;
// resumeIdle restarts the full idle window.
func (t *TurnHandle) pauseIdle() {
	t.watchMu.Lock()
	if t.idleTimer != nil {
		t.idleTimer.Stop()
	}
	t.watchMu.Unlock()
}

func (t *TurnHandle) resumeIdle() {
	t.watchMu.Lock()
	if t.idleTimer != nil && !t.watchersStopped {
		t.idleTimer.Reset(t.idleTimeout)
	}
	t.watchMu.Unlock()
}

func (t *TurnHandle) stopWatchers() {
	t.watchMu.Lock()
	t.watchersStopped = true
	if t.idleTimer != nil {
		t.idleTimer.Stop()
		t.idleTimer = nil
	}
	if t.escalation != nil {
		t.escalation.Stop()
		t.escalation = nil
	}
	stop := t.stopCtxWatch
	t.stopCtxWatch = nil
	t.watchMu.Unlock()
	if stop != nil {
		stop()
	}
}
