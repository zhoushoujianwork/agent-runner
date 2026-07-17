package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	runner "github.com/zhoushoujianwork/agent-runner"
)

const defaultTerminationGrace = 2 * time.Second

type Option func(*Executor)

type Executor struct {
	terminationGrace time.Duration
	inheritEnv       bool
	stripEnv         []string
}

func New(options ...Option) *Executor {
	executor := &Executor{
		terminationGrace: defaultTerminationGrace,
		inheritEnv:       true,
		stripEnv:         []string{"CLAUDECODE"},
	}
	for _, option := range options {
		option(executor)
	}
	return executor
}

func WithTerminationGrace(grace time.Duration) Option {
	return func(executor *Executor) { executor.terminationGrace = grace }
}

func WithInheritedEnvironment(enabled bool) Option {
	return func(executor *Executor) { executor.inheritEnv = enabled }
}

func WithStrippedEnvironment(keys ...string) Option {
	return func(executor *Executor) { executor.stripEnv = append([]string(nil), keys...) }
}

func (e *Executor) Start(ctx context.Context, spec runner.CommandSpec) (runner.Process, error) {
	if ctx == nil {
		return nil, errors.New("host executor: nil context")
	}
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return nil, errors.New("host executor: empty argv")
	}

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = e.environment(spec.Env)
	var stdinWriter io.WriteCloser
	if spec.Interactive {
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("host executor stdin: %w", err)
		}
		stdinWriter = pipe
	} else {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	configureProcess(cmd)

	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("host executor stdout: %w", err)
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return nil, fmt.Errorf("host executor stderr: %w", err)
	}
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = stderr.Close()
		_ = stderrWriter.Close()
		return nil, fmt.Errorf("host executor start %q: %w", spec.Argv[0], err)
	}
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()

	process := &hostProcess{
		cmd:    cmd,
		stdin:  stdinWriter,
		stdout: stdout,
		stderr: stderr,
		grace:  e.terminationGrace,
		done:   make(chan struct{}),
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

func (e *Executor) environment(overrides map[string]string) []string {
	values := make(map[string]string)
	if e.inheritEnv {
		for _, entry := range os.Environ() {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				values[key] = value
			}
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	for _, key := range e.stripEnv {
		delete(values, key)
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

type hostProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
	grace  time.Duration
	done   chan struct{}

	cancelOnce sync.Once
	mu         sync.RWMutex
	status     runner.ExitStatus
	waitErr    error
}

func (p *hostProcess) Stdout() io.Reader { return p.stdout }
func (p *hostProcess) Stderr() io.Reader { return p.stderr }

// StdinWriter exposes the live stdin pipe of an interactive process (nil-safe
// no-op writer when the spec was not interactive is never needed: the runner
// checks the capability before use).
func (p *hostProcess) StdinWriter() io.WriteCloser { return p.stdin }

func (p *hostProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *hostProcess) Wait() (runner.ExitStatus, error) {
	<-p.done
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.status, p.waitErr
}

func (p *hostProcess) Cancel() error {
	if p == nil {
		return nil
	}
	var cancelErr error
	p.cancelOnce.Do(func() {
		cancelErr = terminateProcess(p.cmd, p.grace, p.done)
	})
	return cancelErr
}

func (p *hostProcess) reap() {
	err := p.cmd.Wait()
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
