package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	runner "github.com/zhoushoujianwork/agent-runner"
	"github.com/zhoushoujianwork/agent-runner/engine/claude"
	"github.com/zhoushoujianwork/agent-runner/executor/host"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return errors.New("command is required")
	}
	switch args[0] {
	case "run":
		return runTurn(args[1:], stdout)
	case "doctor":
		return runDoctor(args[1:], stdout)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runTurn(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	requestPath := flags.String("request", "", "request JSON file, or - for stdin")
	prompt := flags.String("prompt", "", "prompt text")
	cwd := flags.String("cwd", "", "working directory")
	model := flags.String("model", "", "Claude model")
	resume := flags.String("resume", "", "Claude session ID")
	newSession := flags.String("new-session", "", "set a new Claude session UUID")
	permission := flags.String("permission", string(runner.PermissionDefault), "default|accept-edits|auto|bypass|manual|dont-ask|plan")
	wallTimeout := flags.Duration("wall-timeout", 30*time.Minute, "maximum run duration; 0 disables")
	idleTimeout := flags.Duration("idle-timeout", 5*time.Minute, "maximum time without output; 0 disables")
	claudeBin := flags.String("claude-bin", "claude", "Claude executable")
	maxTurns := flags.Int("max-turns", 0, "maximum agent turns")
	allowedTools := flags.String("allowed-tools", "", "comma-separated allowed tools")
	disallowedTools := flags.String("disallowed-tools", "", "comma-separated disallowed tools")
	var env keyValues
	var extraArgs stringsFlag
	flags.Var(&env, "env", "environment override KEY=VALUE; repeatable")
	flags.Var(&extraArgs, "extra-arg", "extra Claude argument; repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	var request runner.Request
	var err error
	if *requestPath != "" {
		if *prompt != "" || *cwd != "" || *model != "" || *resume != "" || *newSession != "" || len(env) != 0 || len(extraArgs) != 0 {
			return errors.New("--request cannot be combined with request field flags")
		}
		request, err = readRequest(*requestPath)
		if err != nil {
			return err
		}
	} else {
		request = runner.Request{
			Prompt:          *prompt,
			WorkDir:         *cwd,
			Model:           *model,
			SessionID:       *resume,
			NewSessionID:    *newSession,
			Permission:      runner.PermissionMode(*permission),
			WallTimeout:     *wallTimeout,
			IdleTimeout:     *idleTimeout,
			MaxTurns:        *maxTurns,
			AllowedTools:    splitList(*allowedTools),
			DisallowedTools: splitList(*disallowedTools),
			Env:             map[string]string(env),
			ExtraArgs:       append([]string(nil), extraArgs...),
		}
	}

	r := &runner.Runner{Engine: claude.New(*claudeBin), Executor: host.New()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	handle, err := r.Run(ctx, request)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	for event := range handle.Events() {
		if err := encoder.Encode(event); err != nil {
			handle.Cancel()
			_, _ = handle.Wait()
			return fmt.Errorf("encode event: %w", err)
		}
	}
	_, err = handle.Wait()
	return err
}

func runDoctor(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	claudeBin := flags.String("claude-bin", "claude", "Claude executable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	path, err := exec.LookPath(*claudeBin)
	result := map[string]any{"backend": "host", "claude": *claudeBin, "ok": err == nil}
	if err == nil {
		result["claude_path"] = path
	} else {
		result["error"] = err.Error()
	}
	if encodeErr := json.NewEncoder(stdout).Encode(result); encodeErr != nil {
		return encodeErr
	}
	return err
}

type requestDocument struct {
	Prompt             string                `json:"prompt"`
	WorkDir            string                `json:"cwd"`
	Model              string                `json:"model"`
	AppendSystemPrompt string                `json:"append_system_prompt"`
	SessionID          string                `json:"session_id"`
	NewSessionID       string                `json:"new_session_id"`
	Continue           bool                  `json:"continue"`
	MaxTurns           int                   `json:"max_turns"`
	AllowedTools       []string              `json:"allowed_tools"`
	DisallowedTools    []string              `json:"disallowed_tools"`
	MCPConfig          string                `json:"mcp_config"`
	Permission         runner.PermissionMode `json:"permission"`
	Env                map[string]string     `json:"env"`
	ExtraArgs          []string              `json:"extra_args"`
	WallTimeout        string                `json:"wall_timeout"`
	IdleTimeout        string                `json:"idle_timeout"`
	MaxFrameBytes      int                   `json:"max_frame_bytes"`
	MaxStderrBytes     int                   `json:"max_stderr_bytes"`
}

func readRequest(path string) (runner.Request, error) {
	var reader io.Reader
	if path == "-" {
		reader = os.Stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return runner.Request{}, err
		}
		defer file.Close()
		reader = file
	}
	var document requestDocument
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return runner.Request{}, fmt.Errorf("decode request: %w", err)
	}
	request := runner.Request{
		Prompt: document.Prompt, WorkDir: document.WorkDir, Model: document.Model,
		AppendSystemPrompt: document.AppendSystemPrompt, SessionID: document.SessionID,
		NewSessionID: document.NewSessionID,
		Continue:     document.Continue, MaxTurns: document.MaxTurns,
		AllowedTools: document.AllowedTools, DisallowedTools: document.DisallowedTools,
		MCPConfig: document.MCPConfig, Permission: document.Permission,
		Env: document.Env, ExtraArgs: document.ExtraArgs,
		MaxFrameBytes: document.MaxFrameBytes, MaxStderrBytes: document.MaxStderrBytes,
	}
	var err error
	if request.WallTimeout, err = parseDuration(document.WallTimeout); err != nil {
		return runner.Request{}, fmt.Errorf("wall_timeout: %w", err)
	}
	if request.IdleTimeout, err = parseDuration(document.IdleTimeout); err != nil {
		return runner.Request{}, fmt.Errorf("idle_timeout: %w", err)
	}
	return request, nil
}

func parseDuration(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	return time.ParseDuration(value)
}

type keyValues map[string]string

func (v *keyValues) String() string { return "" }
func (v *keyValues) Set(value string) error {
	key, item, ok := strings.Cut(value, "=")
	if !ok || key == "" {
		return fmt.Errorf("expected KEY=VALUE, got %q", value)
	}
	if *v == nil {
		*v = make(map[string]string)
	}
	(*v)[key] = item
	return nil
}

type stringsFlag []string

func (v *stringsFlag) String() string { return strings.Join(*v, ",") }
func (v *stringsFlag) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func splitList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := parts[:0]
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func printUsage(out io.Writer) {
	fmt.Fprintln(out, "usage: agent-runner <run|doctor|version> [options]")
}
