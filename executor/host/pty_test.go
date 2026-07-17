//go:build !windows

package host_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/agent-runner/engine/claude"
	"github.com/zhoushoujianwork/agent-runner/executor/host"
	runner "github.com/zhoushoujianwork/agent-runner/runner"
)

func buildFakeTUI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-tui")
	cmd := exec.Command("go", "build", "-o", path, "../../internal/faketui")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake tui: %v\n%s", err, output)
	}
	return path
}

// readUntil reads from r until substr appears or the deadline elapses.
func readUntil(t *testing.T, r *bufio.Reader, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var sb strings.Builder
	for time.Now().Before(deadline) {
		b, err := r.ReadByte()
		if err != nil {
			t.Fatalf("read %q so far, then error: %v", sb.String(), err)
		}
		sb.WriteByte(b)
		if strings.Contains(sb.String(), substr) {
			return sb.String()
		}
	}
	t.Fatalf("timed out waiting for %q; got %q", substr, sb.String())
	return ""
}

func newTermRunner(bin string) *runner.Runner {
	return &runner.Runner{
		Engine:   claude.New(bin),
		Executor: host.New(host.WithTerminationGrace(200 * time.Millisecond)),
	}
}

func TestOpenTermRoundTrip(t *testing.T) {
	term, err := newTermRunner(buildFakeTUI(t)).OpenTerm(context.Background(), runner.TermRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	out := bufio.NewReader(term.Output())
	readUntil(t, out, "> ", 5*time.Second)
	if _, err := term.Input().Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	got := readUntil(t, out, "ANSWER:ping", 5*time.Second)
	if !strings.Contains(got, "ANSWER:ping") {
		t.Fatalf("round-trip failed: %q", got)
	}
}

func TestOpenTermResize(t *testing.T) {
	term, err := newTermRunner(buildFakeTUI(t)).OpenTerm(context.Background(), runner.TermRequest{
		Size: runner.TermSize{Cols: 80, Rows: 24},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	out := bufio.NewReader(term.Output())
	readUntil(t, out, "> ", 5*time.Second)
	if err := term.Resize(runner.TermSize{Cols: 132, Rows: 40}); err != nil {
		t.Fatal(err)
	}
	got := readUntil(t, out, "SIZE:132x40", 5*time.Second)
	if !strings.Contains(got, "SIZE:132x40") {
		t.Fatalf("resize not observed: %q", got)
	}
}

// drain copies Output to /dev/null in the background, mirroring a real
// terminal caller (io.Copy to a socket). Without a live reader a PTY child can
// block on writes and outlive the terminate grace.
func drain(term *runner.Term) {
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := term.Output().Read(buf); err != nil {
				return
			}
		}
	}()
}

func TestOpenTermCloseGraceful(t *testing.T) {
	term, err := newTermRunner(buildFakeTUI(t)).OpenTerm(context.Background(), runner.TermRequest{})
	if err != nil {
		t.Fatal(err)
	}
	out := bufio.NewReader(term.Output())
	readUntil(t, out, "> ", 5*time.Second)
	drain(term)

	if err := term.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := term.Close(); err != nil { // idempotent
		t.Fatalf("second close: %v", err)
	}
	select {
	case <-term.Dead():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not die after Close")
	}
	exit, _ := term.Exit()
	if exit.ExitCode != 0 {
		t.Fatalf("graceful exit expected 0, got %+v", exit)
	}
}

func TestOpenTermCloseForced(t *testing.T) {
	term, err := newTermRunner(buildFakeTUI(t)).OpenTerm(context.Background(), runner.TermRequest{
		Env:        map[string]string{"FAKE_IGNORE_SIGTERM": "1"},
		CloseGrace: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := bufio.NewReader(term.Output())
	readUntil(t, out, "> ", 5*time.Second)

	if err := term.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-term.Dead():
	case <-time.After(5 * time.Second):
		t.Fatal("process did not die after Close escalation")
	}
	exit, _ := term.Exit()
	if exit.Signal != "killed" {
		t.Fatalf("forced exit expected SIGKILL, got %+v", exit)
	}
}

func TestOpenTermArgvHasNoPromptOrProtocol(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args.json")
	term, err := newTermRunner(buildFakeTUI(t)).OpenTerm(context.Background(), runner.TermRequest{
		Model:           "claude-test",
		ResumeSessionID: "sess-abc",
		Env:             map[string]string{"FAKE_ARGS_PATH": argsPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	readUntil(t, bufio.NewReader(term.Output()), "> ", 5*time.Second)

	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, banned := range []string{"--print", "--output-format", "--input-format", "--verbose", "--include-partial-messages"} {
		if strings.Contains(joined, banned) {
			t.Errorf("TUI argv must not carry %q: %v", banned, args)
		}
	}
	if !strings.Contains(joined, "--resume sess-abc") || !strings.Contains(joined, "--model claude-test") {
		t.Errorf("session flags missing from TUI argv: %v", args)
	}
}

func TestOpenTermInjectsTerm(t *testing.T) {
	termPath := filepath.Join(t.TempDir(), "term.txt")
	term, err := newTermRunner(buildFakeTUI(t)).OpenTerm(context.Background(), runner.TermRequest{
		Env: map[string]string{"FAKE_TERM_PATH": termPath},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	readUntil(t, bufio.NewReader(term.Output()), "> ", 5*time.Second)

	data, _ := os.ReadFile(termPath)
	if string(data) != "xterm-256color" {
		t.Fatalf("TERM default expected xterm-256color, got %q", data)
	}
}

func TestOpenTermTermOverride(t *testing.T) {
	termPath := filepath.Join(t.TempDir(), "term.txt")
	term, err := newTermRunner(buildFakeTUI(t)).OpenTerm(context.Background(), runner.TermRequest{
		Env: map[string]string{"FAKE_TERM_PATH": termPath, "TERM": "screen-256color"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	readUntil(t, bufio.NewReader(term.Output()), "> ", 5*time.Second)

	data, _ := os.ReadFile(termPath)
	if string(data) != "screen-256color" {
		t.Fatalf("TERM override expected screen-256color, got %q", data)
	}
}

func TestOpenTermExtraDirsLifecycle(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, ".claude", "skills", "review")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	link := filepath.Join(work, ".claude", "skills", "review")

	term, err := newTermRunner(buildFakeTUI(t)).OpenTerm(context.Background(), runner.TermRequest{
		WorkDir:   work,
		ExtraDirs: []runner.ExtraDir{{Source: root}},
	})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, bufio.NewReader(term.Output()), "> ", 5*time.Second)
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("extra-dir link not created while alive: %v", err)
	}

	_ = term.Close()
	<-term.Dead()
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("extra-dir link not cleaned up after death: %v", err)
	}
}
