# agent-runner

`agent-runner` is a small Go SDK and NDJSON CLI for running headless coding
agents with swappable execution backends.

The first milestone supports the real Claude Code CLI on the local host. The
public boundary already separates the Claude protocol (`Engine`) from process
placement (`Executor`), so Docker and remote sandbox backends can be added
without copying session or stream parsing logic.

## Status

Implemented in the host MVP:

- `claude --print --output-format stream-json` command construction
- full-frame preservation plus normalized text, thinking, tool, usage and result events
- session resume/continue and explicit permission modes
- asynchronous `RunHandle` with `Events`, `Wait` and `Cancel`
- wall and idle timeouts
- Unix process-group termination (`SIGTERM`, grace period, then `SIGKILL`)
- bounded, basic secret-redacted stderr capture
- shell-free argv execution and `CLAUDECODE` environment stripping
- fake-Claude contract tests that consume no model quota

Not implemented yet: Docker, remote sandbox, approval broker, persistent session
store, Windows Job Objects, provider failover and VCS/Issue orchestration.

## Go SDK

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    runner "github.com/zhoushoujianwork/agent-runner"
    "github.com/zhoushoujianwork/agent-runner/engine/claude"
    "github.com/zhoushoujianwork/agent-runner/executor/host"
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
Request
  -> Claude Engine: CommandSpec + per-run stream parser
  -> Executor
       -> host (implemented)
       -> docker (next)
       -> sandbox (future)
  -> normalized Event stream + Result
```

The runner deliberately does not own rooms, issues, Git branches, credentials,
session persistence or approval policy. Those remain with BBClaw, ClawFlow,
Agent Room or another calling control plane.

## Development

```bash
go test ./...
go vet ./...
```

The repository is MIT licensed.
