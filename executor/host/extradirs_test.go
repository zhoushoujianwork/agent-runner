package host

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	runner "github.com/zhoushoujianwork/agent-runner/runner"
)

func TestPlaceExtraDirsDefaultTargetAndCleanup(t *testing.T) {
	work := t.TempDir()
	src := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	created, err := placeExtraDirs(work, []runner.ExtraDir{{Source: src}})
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(work, ".claude", "skills")
	dest, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected symlink at %s: %v", link, err)
	}
	srcAbs, _ := filepath.Abs(src)
	if dest != srcAbs {
		t.Fatalf("link points to %q, want %q", dest, srcAbs)
	}
	if len(created) != 1 || created[0] != link {
		t.Fatalf("unexpected created links: %v", created)
	}
	removeLinks(created)
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("link not cleaned up: %v", err)
	}
}

func TestPlaceExtraDirsExplicitTarget(t *testing.T) {
	work := t.TempDir()
	src := t.TempDir()
	created, err := placeExtraDirs(work, []runner.ExtraDir{{Source: src, Target: ".claude/agents"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(work, ".claude", "agents")); err != nil {
		t.Fatalf("expected link: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("unexpected created: %v", created)
	}
}

func TestPlaceExtraDirsRejectsEscape(t *testing.T) {
	work := t.TempDir()
	src := t.TempDir()
	for _, target := range []string{"../escape", "/abs/target", "sub/../../escape"} {
		if _, err := placeExtraDirs(work, []runner.ExtraDir{{Source: src, Target: target}}); err == nil {
			t.Fatalf("target %q should be rejected", target)
		}
	}
}

func TestPlaceExtraDirsMissingSource(t *testing.T) {
	work := t.TempDir()
	if _, err := placeExtraDirs(work, []runner.ExtraDir{{Source: filepath.Join(t.TempDir(), "nope")}}); err == nil {
		t.Fatal("missing source must fail")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := placeExtraDirs(work, []runner.ExtraDir{{Source: file}}); err == nil {
		t.Fatal("non-directory source must fail")
	}
}

func TestPlaceExtraDirsConflict(t *testing.T) {
	work := t.TempDir()
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(work, ".claude", filepath.Base(src)), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := placeExtraDirs(work, []runner.ExtraDir{{Source: src}})
	var runErr *runner.RunError
	if !errors.As(err, &runErr) || runErr.Kind != runner.ErrorStart {
		t.Fatalf("expected ErrorStart, got %v", err)
	}
}

func TestPlaceExtraDirsIdempotentAdoption(t *testing.T) {
	work := t.TempDir()
	src := t.TempDir()
	// First placement creates the link.
	first, err := placeExtraDirs(work, []runner.ExtraDir{{Source: src}})
	if err != nil || len(first) != 1 {
		t.Fatalf("first placement failed: %v %v", first, err)
	}
	// Second placement adopts it: no new links created, so nothing to clean.
	second, err := placeExtraDirs(work, []runner.ExtraDir{{Source: src}})
	if err != nil {
		t.Fatalf("idempotent placement failed: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("adopted link must not be owned: %v", second)
	}
}

func TestPlaceExtraDirsRollbackOnFailure(t *testing.T) {
	work := t.TempDir()
	good := t.TempDir()
	created, err := placeExtraDirs(work, []runner.ExtraDir{
		{Source: good, Target: ".claude/good"},
		{Source: filepath.Join(t.TempDir(), "missing")},
	})
	if err == nil {
		t.Fatal("expected failure on missing source")
	}
	if created != nil {
		t.Fatalf("failed placement must return no links: %v", created)
	}
	if _, statErr := os.Lstat(filepath.Join(work, ".claude", "good")); !os.IsNotExist(statErr) {
		t.Fatalf("first link must be rolled back: %v", statErr)
	}
}
