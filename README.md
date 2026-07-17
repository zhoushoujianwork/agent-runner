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
internal/faketui/     scriptable fake TUI process for PTY contract tests
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
- extra dirs: point at a context root (a project checkout) and its
  `.claude`/`.agent` skills, agents and commands are merged into the process
  working directory entry by entry, so the CLI's own discovery picks them up;
  local entries win, links are removed on exit unless `Keep`
- TUI/PTY term mode (`Runner.OpenTerm`): the interactive CLI runs inside a
  pseudo-terminal with raw byte-stream Input/Output/Resize — zero-parse
  conduit for terminal mirroring (xterm.js), sharing session/resume
  semantics and ExtraDirs with headless runs (see docs/term-mode.md;
  Unix only for now)
- fake-Claude and fake-TUI contract tests that consume no model quota

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
    WorkDir:         "/workspaces/task-42",
    TurnIdleTimeout: 5 * time.Minute,
    // Project agent context appears inside the workspace for the lifetime of
    // the process: the root's .claude/.agent skills, agents and commands are
    // merged entry by entry (local entries win), so the CLI's own discovery
    // mechanism picks them up. Links are removed on exit unless Keep is set;
    // Target switches to an exact single link.
    ExtraDirs: []runner.ExtraDir{
        {Source: "/repos/myproj"},                             // discovery mode
        {Source: "/shared/context", Target: ".claude/shared"}, // exact mode
    },
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

Project context via `ExtraDirs`: when the agent's `WorkDir` is not the project
directory itself (e.g. an isolated workspace), point at the context roots to
expose inside it. The executor discovers each root's `.claude/` and `.agent/`
convention dirs and links the entries of their `skills/`, `agents/` and
`commands/` into the same relative place under `WorkDir`, entry by entry, so
the agent CLI's own discovery mechanism picks them up.

```go
handle, err := r.Run(ctx, runner.Request{
    WorkDir: "/workspaces/task-42",
    ExtraDirs: []runner.ExtraDir{
        {Source: "/repos/myproj"},                             // discovery mode
        {Source: "/repos/lib", Keep: true},                    // links survive exit
        {Source: "/shared/context", Target: ".claude/shared"}, // exact mode
    },
})
```

In discovery mode local entries win (skipped silently), identical existing
links are adopted without ownership, and a root without convention dirs is a
silent no-op. Setting `Target` (relative to `WorkDir`, no escaping) switches to
exact mode: `Source` itself is linked verbatim and an existing `Target` is an
error. Links created by the run are removed when the process exits unless
`Keep` is set; adopted links are never touched. Placement is the executor
backend's concern — host uses symlinks, a future docker backend would use bind
mounts — so the Engine never sees it. When several sessions share one
`WorkDir`, whoever created a link cleans it up.

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
  --cwd /workspaces/task-42 \
  --permission default \
  --extra-dir /repos/myproj \
  --extra-dir /shared/context=.claude/shared \
  --prompt 'Find the failing test and explain it'
```

`--extra-dir SOURCE[=TARGET]` is repeatable and mirrors the SDK `ExtraDirs`
field: bare `SOURCE` is a context root scanned for `.claude`/`.agent` content,
`SOURCE=TARGET` is an exact link.

The CLI writes one JSON event per stdout line. Human-readable terminal errors
go to stderr.

For automation, pass a request document:

```bash
printf '%s\n' '{
  "prompt": "Run the tests and fix the failure",
  "cwd": "/path/to/repository",
  "permission": "bypass",
  "wall_timeout": "30m",
  "idle_timeout": "5m",
  "extra_dirs": [
    {"source": "/repos/myproj/.claude/skills"},
    {"source": "/shared/agents", "target": ".claude/agents"}
  ]
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
