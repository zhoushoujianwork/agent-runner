package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	runner "github.com/zhoushoujianwork/agent-runner/runner"
)

func TestReadRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	data := []byte(`{
  "prompt": "hello",
  "cwd": "/workspace",
  "permission": "accept-edits",
  "wall_timeout": "2m",
  "idle_timeout": "15s"
}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	request, err := readRequest(path)
	if err != nil {
		t.Fatal(err)
	}
	if request.Prompt != "hello" || request.WorkDir != "/workspace" || request.Permission != runner.PermissionAcceptEdits {
		t.Fatalf("unexpected request: %+v", request)
	}
	if request.WallTimeout != 2*time.Minute || request.IdleTimeout != 15*time.Second {
		t.Fatalf("unexpected timeouts: %+v", request)
	}
}

func TestReadRequestRejectsUnknownField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, []byte(`{"prompt":"hello","surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRequest(path); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestReadRequestExtraDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request.json")
	data := []byte(`{
  "prompt": "hi",
  "extra_dirs": [
    {"source": "/repos/p/.claude/skills"},
    {"source": "/shared/agents", "target": ".claude/agents"}
  ]
}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	request, err := readRequest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.ExtraDirs) != 2 {
		t.Fatalf("expected 2 extra dirs, got %+v", request.ExtraDirs)
	}
	if request.ExtraDirs[0].Source != "/repos/p/.claude/skills" || request.ExtraDirs[0].Target != "" {
		t.Fatalf("unexpected first extra dir: %+v", request.ExtraDirs[0])
	}
	if request.ExtraDirs[1].Target != ".claude/agents" {
		t.Fatalf("unexpected second extra dir: %+v", request.ExtraDirs[1])
	}
}

func TestExtraDirsFlagParsing(t *testing.T) {
	var flag extraDirsFlag
	if err := flag.Set("/src/skills"); err != nil {
		t.Fatal(err)
	}
	if err := flag.Set("/src/agents=.claude/agents"); err != nil {
		t.Fatal(err)
	}
	if len(flag) != 2 {
		t.Fatalf("expected 2 entries, got %+v", flag)
	}
	if flag[0].Source != "/src/skills" || flag[0].Target != "" {
		t.Fatalf("unexpected entry 0: %+v", flag[0])
	}
	if flag[1].Source != "/src/agents" || flag[1].Target != ".claude/agents" {
		t.Fatalf("unexpected entry 1: %+v", flag[1])
	}
	if err := flag.Set("=only-target"); err == nil {
		t.Fatal("empty source must be rejected")
	}
}
