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
