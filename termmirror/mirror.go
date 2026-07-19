// Package termmirror turns a TUI byte stream into semantic events without
// touching the raw passthrough: it tees the terminal output into a server-side
// VT screen (termscreen) and, at each output chunk and on a silence tick, asks
// the engine's TermObserver (runner.TermSemantics) what the screen means —
// newest reply text, turn boundary, blocking menus. Callers keep forwarding
// the raw bytes to their terminal clients (xterm.js) exactly as before.
package termmirror

import (
	"io"
	"sync"
	"time"

	"github.com/zhoushoujianwork/agent-runner/runner"
	"github.com/zhoushoujianwork/agent-runner/termscreen"
)

const (
	defaultCols    = 120
	defaultRows    = 32
	defaultTick    = 100 * time.Millisecond
	defaultBacklog = 64
)

// Options configures a Mirror.
type Options struct {
	// Size is the screen grid; it must match the PTY geometry the process was
	// started with. Zero defaults to 120x32 (agent-runner's PTY default).
	Size runner.TermSize
	// OnRaw, when set, receives every output chunk before it is interpreted —
	// the raw passthrough for terminal clients. It runs on the pump goroutine:
	// do not block (fan out through your own buffered channels).
	OnRaw func(chunk []byte)
	// Tick is the silence-tick interval that drives turn-boundary detection
	// between output chunks. Zero defaults to 100ms.
	Tick time.Duration
	// Backlog is the Events channel buffer; observations beyond it are dropped
	// (the next observation supersedes them). Zero defaults to 64.
	Backlog int
}

// New starts mirroring output. The engine must implement runner.TermSemantics
// or ErrBackendUnsupported is returned. The pump stops (and Events closes)
// when output reaches EOF — for a runner.Term that is process exit.
func New(output io.Reader, engine runner.Engine, opts Options) (*Mirror, error) {
	semantics, ok := engine.(runner.TermSemantics)
	if !ok {
		return nil, runner.ErrBackendUnsupported
	}
	if opts.Size.Cols == 0 {
		opts.Size.Cols = defaultCols
	}
	if opts.Size.Rows == 0 {
		opts.Size.Rows = defaultRows
	}
	if opts.Tick <= 0 {
		opts.Tick = defaultTick
	}
	if opts.Backlog <= 0 {
		opts.Backlog = defaultBacklog
	}

	screen := termscreen.New(int(opts.Size.Cols), int(opts.Size.Rows))
	m := &Mirror{
		output:   output,
		onRaw:    opts.OnRaw,
		tick:     opts.Tick,
		screen:   screen,
		observer: semantics.NewTermObserver(screen),
		events:   make(chan runner.TermObservation, opts.Backlog),
		done:     make(chan struct{}),
	}
	go m.pump()
	go m.tickLoop()
	return m, nil
}

// Mirror is one live screen mirror plus its semantic observer.
type Mirror struct {
	output io.Reader
	onRaw  func([]byte)
	tick   time.Duration

	mu        sync.Mutex
	screen    *termscreen.Screen
	observer  runner.TermObserver
	lastEnded bool
	lastSig   string // last emitted prompt signature ("" = none)

	events chan runner.TermObservation
	done   chan struct{}
	closed sync.Once
}

// Events streams semantic observations: one per reply change, per new blocking
// menu, and per turn-end edge. Closed after the output stream ends.
func (m *Mirror) Events() <-chan runner.TermObservation { return m.events }

// NewTurn resets per-turn semantics (extraction baseline, boundary clock).
// Call right after writing a user turn into the terminal input.
func (m *Mirror) NewTurn() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observer.NewTurn()
	m.lastEnded = false
	m.lastSig = ""
}

// VisibleText is the current visible grid as plain text.
func (m *Mirror) VisibleText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.screen.VisibleText()
}

// Snapshot renders the current screen as ANSI bytes for replay into a joining
// terminal client.
func (m *Mirror) Snapshot() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.screen.Snapshot()
}

// ScrollbackChunks returns up to n recent scrollback lines as raw chunks for
// client replay ahead of Snapshot.
func (m *Mirror) ScrollbackChunks(n int) [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.screen.ScrollbackChunks(n)
}

// Resize updates the mirrored grid; keep it in lockstep with Term.Resize.
func (m *Mirror) Resize(size runner.TermSize) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.screen.Resize(int(size.Cols), int(size.Rows))
}

func (m *Mirror) pump() {
	defer m.finish()
	buf := make([]byte, 4096)
	for {
		n, err := m.output.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if m.onRaw != nil {
				m.onRaw(chunk)
			}
			m.mu.Lock()
			m.screen.Feed(chunk)
			m.observe(time.Now(), true)
			m.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (m *Mirror) tickLoop() {
	t := time.NewTicker(m.tick)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case now := <-t.C:
			m.mu.Lock()
			m.observe(now, false)
			m.mu.Unlock()
		}
	}
}

// observe samples the screen under m.mu and emits an event when something a
// consumer would act on changed: new reply text, a new blocking menu, or the
// turn-end edge. A full Events buffer drops the observation — the next one
// carries the fresher state.
func (m *Mirror) observe(now time.Time, hadBytes bool) {
	obs := m.observer.Observe(now, hadBytes)
	endedEdge := obs.TurnEnded && !m.lastEnded
	m.lastEnded = obs.TurnEnded

	sig := ""
	if obs.Prompt != nil {
		sig = obs.Prompt.Signature
	}
	promptEdge := sig != "" && sig != m.lastSig
	if sig != "" {
		m.lastSig = sig
	}

	if !obs.ReplyChanged && !endedEdge && !promptEdge {
		return
	}
	obs.TurnEnded = endedEdge
	select {
	case m.events <- obs:
	default: // slow consumer: drop; the next observation supersedes
	}
}

func (m *Mirror) finish() {
	m.closed.Do(func() {
		close(m.done)
		close(m.events)
	})
}
