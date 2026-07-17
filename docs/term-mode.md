# TUI/PTY 启动模式设计(Term Mode)

Issue: [#10](https://github.com/zhoushoujianwork/agent-runner/issues/10)
状态: M1 已实施
目标版本: v0.6.0

## 1. 背景

bbclaw adapter_v2 的整合终态是单一 adapter、双启动模式:

| 模式 | 进程形态 | 通道 | 语义 |
|---|---|---|---|
| headless | `claude --print --input-format stream-json`(现有会话) | 设备/语音/审批 | 结构化事件、OnPermission、优雅中断 |
| **TUI** | `claude` 交互式 TUI 跑在 **PTY** 里 | 手机/web xterm.js 终端镜像 | 原始终端字节双向透传 |

TUI 模式目前由 bbclaw 自己的 `ptyhost`(creack/pty 拉起)+ `butler.DeviceClaudeArgs`(手拼 argv)承担。
「进程如何被构造与放置」是 agent-runner Engine/Executor 的职责边界,应下沉到 SDK:

- **argv 构造与 headless 共享一套语义**:resume/session-id/model/permission/append-system-prompt 两种模式行为一致,headless 跑的会话 TUI 能 `--resume` 接着看,反之亦然;
- **ExtraDirs / env 清洗 / 进程组回收**免费复用;
- bbclaw 删掉 ptyhost 与 argv 拼装,只保留它真正的价值:VT 仿真与语义抽取。

## 2. 非目标

- **不做 VT 仿真、抓屏、turn 边界检测、审批提示识别**——TUI 字节流的语义解释留给调用方(bbclaw 的 vtscreen/extract);
- **不做 TUI 模式的 OnPermission**——TUI 有自己的交互式审批 UI,字节流里自然呈现;
- **不做跨模式会话注册表**——headless↔TUI 互通依赖 claude 自身的 session id/JSONL 机制,SDK 只保证两种模式的 resume 旗标语义一致;
- **不做 stderr 分离**——PTY 天然合流 stdout/stderr,诊断降级为字节流的一部分(取舍已被 bbclaw v2 DESIGN.md §2 接受);
- **不做 turn/Result 聚合**——Term 没有 turn,只有一条字节流和一个退出状态。

## 3. 架构

沿用 Engine × Executor 正交,以「可选能力接口」扩展,不动现有 headless 契约:

```text
Runner.OpenTerm(ctx, TermRequest)
  ├─ Engine ──(类型断言)──▶ TermEngine.NewTerm(TermRequest) → CommandSpec   // argv,无协议
  ├─ Executor ─(类型断言)──▶ PTYExecutor.StartPTY(ctx, spec, size) → PTYProcess
  │        host: creack/pty(新增依赖,~0 传递依赖);ExtraDirs 同 prepare/清理链
  │        docker(future): exec attach with tty
  └─ Term 运行时:Input/Output/Resize/Close/Dead/Exit
```

任一断言失败返回 `ErrBackendUnsupported`,与现有可选能力的处理方式一致。

## 4. 契约草案

```go
// runner/contracts.go 追加

// TermEngine is implemented by engines whose CLI has an interactive TUI mode.
type TermEngine interface {
    // NewTerm builds the interactive-TUI process spec (no --print, no
    // protocol). Session fields carry the same semantics as SessionRequest.
    NewTerm(TermRequest) (CommandSpec, error)
}

// PTYExecutor is implemented by executors that can place a process inside a
// pseudo-terminal.
type PTYExecutor interface {
    StartPTY(ctx context.Context, spec CommandSpec, size TermSize) (PTYProcess, error)
}

// PTYProcess is one live process attached to a PTY. Output carries the merged
// raw terminal byte stream (stdout+stderr, VT escape sequences included).
type PTYProcess interface {
    Input() io.Writer
    Output() io.Reader
    Resize(TermSize) error
    Wait() (ExitStatus, error)
    Cancel() error
    PID() int
}

type TermSize struct {
    Cols, Rows uint16
}
```

```go
// runner/types.go 追加

// TermRequest opens one interactive TUI process in a PTY. Fields shared with
// SessionRequest carry identical semantics so headless and TUI runs of the
// same conversation interoperate via the provider's own session mechanism.
type TermRequest struct {
    WorkDir            string
    Model              string
    AppendSystemPrompt string
    ResumeSessionID    string
    NewSessionID       string
    Continue           bool
    Permission         PermissionMode
    MCPConfig          string
    AllowedTools       []string
    DisallowedTools    []string
    Env                map[string]string
    ExtraArgs          []string
    ExtraDirs          []ExtraDir
    // Size is the initial terminal geometry; zero defaults to 120x32.
    Size TermSize
    // CloseGrace bounds Close's wait for a natural exit after the terminate
    // signal before escalating to kill. Zero uses a 3s default.
    CloseGrace time.Duration
}
```

```go
// runner/term.go 新增

func (r *Runner) OpenTerm(ctx context.Context, req TermRequest) (*Term, error)

type Term struct{ ... }

func (t *Term) Input() io.Writer          // 键盘字节写入(xterm.js onData 直通)
func (t *Term) Output() io.Reader         // 原始终端字节(xterm.js write 直通)
func (t *Term) Resize(size TermSize) error
func (t *Term) PID() int
func (t *Term) Dead() <-chan struct{}     // 进程退出且输出耗尽
func (t *Term) Exit() (ExitStatus, error) // Dead 后可读
func (t *Term) Close() error              // SIGTERM → CloseGrace → SIGKILL;幂等
```

设计取舍:

- **Output 用 io.Reader 而非 channel**:调用方(WS 转发)天然按 buffer 读;不强加分帧;
- **Term 不做内部泵**:与 Session 不同,Term 没有解析层,runner 不碰字节——零拷贝直通,唯一职责是生命周期(ExtraDirs 清理挂在进程 reap 上,与 headless 相同机制);
- **Input 不做 StartupInput 定时注入**(bbclaw ptyhost 有跳过 onboarding 的延迟按键):这是调用方策略,拿着 `Input()` 自己写即可;
- **Ctrl-C/ESC 中断**同理属于字节流语义,调用方自己写 `\x1b`。SDK 的 `Close()` 只负责进程级退出。

## 5. Claude TermEngine 映射

`engine/claude` 实现 `TermEngine`,argv 与 headless 的差异仅在协议旗标:

| TermRequest 字段 | TUI argv | 与 headless 差异 |
|---|---|---|
| — | (无 `--print`/`--output-format`/`--input-format`/`--verbose`/`--include-partial-messages`) | TUI 无协议旗标 |
| ResumeSessionID | `--resume <id>` | 同 |
| NewSessionID | `--session-id <id>` | 同 |
| Continue | `--continue` | 同 |
| Model | `--model` | 同 |
| AppendSystemPrompt | `--append-system-prompt` | 同 |
| Permission | `--permission-mode <mode>` | 同(TUI 下审批走自己的交互 UI) |
| MCPConfig | `--mcp-config` | 同 |
| Allowed/DisallowedTools | `--allowedTools`/`--disallowedTools` | 同 |
| ExtraArgs | 追加末尾 | 同 |

互斥校验(resume/new/continue 三选一)与 `NewSession` 共享同一实现。

## 6. host PTY 执行器

`executor/host` 新增 `StartPTY`:

1. `prepareExtraDirs`(与 Start 完全同一条链:死链清扫、发现/精确模式、失败回滚);
2. `pty.StartWithSize`(creack/pty)拉起,`TERM=xterm-256color` 缺省注入(可被 Env 覆盖),沿用 `environment()` 的继承/清洗(`CLAUDECODE` 剥离);
3. 进程组设置与现有 `configureProcess` 一致;`Cancel()` 走现有 SIGTERM→grace→SIGKILL 升级链;
4. reap 时关闭 PTY、移除本次创建的 ExtraDirs 链接(现有机制);
5. `Resize` 转发 `pty.Setsize`;并发安全(PTY fd 上 ioctl 天然原子)。

新增依赖:`github.com/creack/pty`(纯 Go + syscall,无传递依赖)。这是本仓库第一个第三方依赖,收益(免手写各平台 ioctl)明显大于成本。Windows 首期不支持(`ErrBackendUnsupported`,ConPTY 留待后续)。

## 7. bbclaw v2 接入草图(另仓库 issue,非本期)

```go
term, _ := r.OpenTerm(ctx, runner.TermRequest{
    WorkDir:         workspace,
    ResumeSessionID: sess.ActiveID(),
    Permission:      runner.PermissionDefault, // forward-to-device 模式
    ExtraDirs:       []runner.ExtraDir{{Source: projectRoot}},
    Size:            runner.TermSize{Cols: cols, Rows: rows},
})
// 通道①终端镜像: io.Copy(wsConn, term.Output()) / io.Copy(term.Input(), wsConn)
// 通道②语义抽取: vtscreen 改为消费同一份 Output 的 tee(替代 ptyhost)
```

`session.Manager` 的 detach-GC/生命周期不变,只把 `ptyhost.Config` 换成 `TermRequest`。
headless 模式(语义通道换 agent-runner 会话)是 v2 整合的另一半,单独立项。

## 8. 测试策略

- `internal/faketui`:mockcli 风格的假 TUI 进程(打印 `> ` 提示符、回显 `ANSWER:<输入>`、响应 resize 打印 `SIZE:<cols>x<rows>`、SIGTERM 优雅退出);
- 契约测试:PTY round-trip(写入→回显)、Resize 生效、Close 升级链(优雅/强杀两分支)、Dead/Exit 语义、ExtraDirs 在 PTY 模式下的建链/清理、prompt 类输入不进 argv、`TERM` 注入与覆盖;
- 真机冒烟(手动/门控):`claude --resume <headless 会话>` 在 PTY 里能接续同一对话——双模式互通验收。

## 9. 里程碑

| | 内容 | 产物 |
|---|---|---|
| M1 | 契约 + claude TermEngine + host StartPTY + Term 运行时 + faketui 契约测试 | 本仓库 v0.6.0 |
| M2 | bbclaw v2 接入:ptyhost/DeviceClaudeArgs 替换为 OpenTerm | bbclaw issue(另立) |
| M3 | bbclaw v2 headless 模式(语义通道)接 agent-runner 会话,双模式并存 | bbclaw issue(另立) |
