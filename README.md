# agent-runner

`agent-runner` is a small Go SDK and NDJSON CLI for running headless coding
agents with swappable execution backends.

The core runtime is a **bidirectional protocol session**: one persistent agent
process that accepts many turns over stdin, answers permission prompts through
a caller-supplied callback, and supports turn-level interrupts without losing
the warmed-up process. One-shot runs are the degenerate case (open, one turn,
close). The public boundary separates the wire protocol (`Engine`) from
process placement (`Executor`), so Docker and remote sandbox backends can be
added without touching session or stream-parsing logic.

## Layout

```text
cmd/agent-runner/     NDJSON CLI
runner/               core runtime and provider-neutral contracts (public API)
engine/claude/        Claude Code stream-json engine incl. control protocol
executor/host/        host process backend (docker, sandbox: future)
internal/fakeclaude/  scriptable fake Claude CLI for contract tests
```

## Status

Implemented:

- persistent sessions: one `claude --print --input-format stream-json`
  process, many serial turns via `Session.Send`
- one-shot `Runner.Run` built on the same session runtime; it closes stdin
  right after writing its only turn (`Session.CloseInput`), so agents that
  read stdin to EOF before answering never deadlock
- bidirectional control protocol: `can_use_tool` permission prompts answered
  through `SessionRequest.OnPermission`, turn-level interrupt frames
- turn cancellation/idle timeout interrupts the turn first and only kills the
  process when the agent does not comply within `CloseGrace`
- full-frame preservation plus normalized text, thinking, tool, usage and
  result events
- session resume/continue and explicit permission modes
- asynchronous `RunHandle`/`TurnHandle` with `Events` and `Wait`
- wall and idle timeouts, Unix process-group termination, bounded
  secret-redacted stderr capture
- shell-free argv execution and `CLAUDECODE` environment stripping
- fake-Claude contract tests that consume no model quota

Not implemented yet: CLI `serve` mode for sessions, Docker and remote sandbox
executors, persistent session store, Windows Job Objects, provider failover
and VCS/Issue orchestration.

## Go SDK

One-shot run:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/zhoushoujianwork/agent-runner/engine/claude"
    "github.com/zhoushoujianwork/agent-runner/executor/host"
    "github.com/zhoushoujianwork/agent-runner/runner"
)

func main() {
    r := &runner.Runner{
        Engine:   claude.New("claude"),
        Executor: host.New(),
    }
    handle, err := r.Run(context.Background(), runner.Request{
        Prompt:      "Explain this repository",
        WorkDir:     "/path/to/repository",
        Permission:  runner.PermissionDefault,
        WallTimeout: 30 * time.Minute,
        IdleTimeout: 5 * time.Minute,
    })
    if err != nil {
        log.Fatal(err)
    }
    for event := range handle.Events() {
        if event.Type == runner.EventTextDelta || event.Type == runner.EventText {
            fmt.Print(event.Text)
        }
    }
    result, err := handle.Wait()
    if err != nil {
        log.Fatal(err)
    }
    fmt.Printf("\nsession=%s duration=%dms\n", result.SessionID, result.DurationMS)
}
```

Persistent session with permission approval:

```go
session, err := r.OpenSession(ctx, runner.SessionRequest{
    WorkDir:         "/path/to/repository",
    TurnIdleTimeout: 5 * time.Minute,
    OnPermission: func(ctx context.Context, req runner.PermissionRequest) (runner.PermissionDecision, error) {
        // Ask a human, check a policy engine, etc. The turn idle timer is
        // paused while the prompt is pending.
        return runner.PermissionDecision{Allow: true}, nil
    },
})
if err != nil {
    log.Fatal(err)
}
defer session.Close()

<-session.Ready() // optional prewarm barrier

turn, err := session.Send(ctx, runner.TurnInput{Prompt: "Run the tests"})
if err != nil {
    log.Fatal(err)
}
result, err := turn.Wait()
```

Cancelling a turn's context (or hitting its idle timeout) sends the protocol's
interrupt frame first; the session survives when the agent complies within
`CloseGrace`, so the next `Send` reuses the warmed-up process.

`Wait` does not require consuming `Events`; an internal queue prevents the
agent process from blocking. Long-lived callers should still drain or discard
the event channel so the event-pump goroutine can exit.

## CLI

```bash
go build -o bin/agent-runner ./cmd/agent-runner

bin/agent-runner doctor

bin/agent-runner run \
  --cwd /path/to/repository \
  --permission default \
  --prompt 'Find the failing test and explain it'
```

The CLI writes one JSON event per stdout line. Human-readable terminal errors
go to stderr.

For automation, pass a request document:

```bash
printf '%s\n' '{
  "prompt": "Run the tests and fix the failure",
  "cwd": "/path/to/repository",
  "permission": "bypass",
  "wall_timeout": "30m",
  "idle_timeout": "5m"
}' | bin/agent-runner run --request -
```

Permission bypass is never selected implicitly. Use it only when the caller has
established an isolation boundary such as a hardened container.

## Architecture

```text
SessionRequest
  -> Engine (claude): CommandSpec + bidirectional SessionProtocol
       ParseLine -> Step{Events, Reply, Control, EndOfTurn}
       EncodeTurn / EncodeInterrupt / EncodePermissionResponse
  -> Session runtime (runner): read loop, stdin write-back, turn handles,
       permission callback, interrupt-then-kill escalation
  -> Executor
       -> host (implemented)
       -> docker (next)
       -> sandbox (future)
  -> normalized Event stream + per-turn Result

Runner.Run = OpenSession + Send(1 turn) + Close
```

The runner deliberately does not own rooms, issues, Git branches, credentials,
session persistence or approval *policy* (only the approval *transport*).
Those remain with BBClaw, ClawFlow, Agent Room or another calling control
plane.

## Development

```bash
go test ./...
go vet ./...
```

The repository is MIT licensed.
