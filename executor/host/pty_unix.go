//go:build !windows

package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	runner "github.com/zhoushoujianwork/agent-runner/runner"
)

const defaultTermType = "xterm-256color"

// defaultTermSize is applied when a TermRequest leaves the geometry zero.
var defaultTermSize = runner.TermSize{Cols: 120, Rows: 32}

// StartPTY starts spec inside a pseudo-terminal and returns the live process.
// It reuses the same extra-dir preparation, environment cleaning, process-group
// setup and reap/cleanup chain as Start; TERM defaults to xterm-256color and is
// overridable via spec.Env. Output is the merged raw terminal byte stream.
func (e *Executor) StartPTY(ctx context.Context, spec runner.CommandSpec, size runner.TermSize) (runner.PTYProcess, error) {
	if ctx == nil {
		return nil, errors.New("host executor: nil context")
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("host executor: empty argv")
	}
	if size.Cols == 0 {
		size.Cols = defaultTermSize.Cols
	}
	if size.Rows == 0 {
		size.Rows = defaultTermSize.Rows
	}

	links, err := prepareExtraDirs(spec.Dir, spec.ExtraDirs)
	if err != nil {
		return nil, fmt.Errorf("host executor extra dirs: %w", err)
	}

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = e.ptyEnvironment(spec.Env)
	// pty.StartWithSize sets Setsid+Setctty, which already places the child in
	// a new session and process group (pgid == pid). Adding configureProcess's
	// Setpgid on top conflicts with Setsid and fails with EPERM, so the PTY
	// path relies on the session for the group that terminateProcess signals.

	winsize := &pty.Winsize{Cols: size.Cols, Rows: size.Rows}
	ptmx, err := pty.StartWithSize(cmd, winsize)
	if err != nil {
		removeLinks(links)
		return nil, fmt.Errorf("host executor start pty %q: %w", spec.Argv[0], err)
	}

	process := &ptyProcess{
		cmd:   cmd,
		ptmx:  ptmx,
		grace: e.terminationGrace,
		links: links,
		done:  make(chan struct{}),
	}
	go process.reap()
	go func() {
		select {
		case <-ctx.Done():
			_ = process.Cancel()
		case <-process.done:
		}
	}()
	return process, nil
}

// ptyEnvironment layers a default TERM under the executor's cleaned
// environment; spec.Env (already merged by environment) can override it.
func (e *Executor) ptyEnvironment(overrides map[string]string) []string {
	merged := map[string]string{"TERM": defaultTermType}
	for key, value := range overrides {
		merged[key] = value
	}
	return e.environment(merged)
}

type ptyProcess struct {
	cmd   *exec.Cmd
	ptmx  *os.File
	grace time.Duration
	links []string
	done  chan struct{}

	cancelOnce sync.Once
	mu         sync.RWMutex
	status     runner.ExitStatus
	waitErr    error
}

func (p *ptyProcess) Input() io.Writer  { return p.ptmx }
func (p *ptyProcess) Output() io.Reader { return ptyReader{p.ptmx} }

func (p *ptyProcess) Resize(size runner.TermSize) error {
	if p == nil || p.ptmx == nil {
		return errors.New("host pty: no terminal")
	}
	if size.Cols == 0 {
		size.Cols = defaultTermSize.Cols
	}
	if size.Rows == 0 {
		size.Rows = defaultTermSize.Rows
	}
	return pty.Setsize(p.ptmx, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
}

func (p *ptyProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *ptyProcess) Wait() (runner.ExitStatus, error) {
	<-p.done
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status, p.waitErr
}

func (p *ptyProcess) Cancel() error {
	if p == nil {
		return nil
	}
	var cancelErr error
	p.cancelOnce.Do(func() {
		cancelErr = terminateProcess(p.cmd, p.grace, p.done)
	})
	return cancelErr
}

func (p *ptyProcess) reap() {
	err := p.cmd.Wait()
	_ = p.ptmx.Close()
	removeLinks(p.links)
	status := runner.ExitStatus{ExitCode: -1}
	if p.cmd.ProcessState != nil {
		status.ExitCode = p.cmd.ProcessState.ExitCode()
		status.Signal = processSignal(p.cmd.ProcessState)
	}
	if _, ok := err.(*exec.ExitError); ok {
		err = nil
	}
	p.mu.Lock()
	p.status = status
	p.waitErr = err
	p.mu.Unlock()
	close(p.done)
}

// ptyReader adapts the PTY master so a slave-closed read surfaces as EOF. On
// Linux a read after the child exits returns EIO; callers copying Output want
// a clean io.EOF instead.
type ptyReader struct{ f *os.File }

func (r ptyReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if err != nil && n == 0 {
		if pe, ok := err.(*os.PathError); ok && errors.Is(pe.Err, syscall.EIO) {
			return 0, io.EOF
		}
	}
	return n, err
}
