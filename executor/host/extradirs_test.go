package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/agent-runner/runner"
)

func TestPrepareExtraDirsDefaultTarget(t *testing.T) {
	source := t.TempDir()
	work := t.TempDir()
	links, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: source}})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(work, ".claude", filepath.Base(source))
	if len(links) != 1 || links[0] != want {
		t.Fatalf("links = %v, want [%s]", links, want)
	}
	dest, err := os.Readlink(want)
	if err != nil {
		t.Fatal(err)
	}
	if dest != source {
		t.Fatalf("link dest = %q, want %q", dest, source)
	}
	removeLinks(links)
	if _, err := os.Lstat(want); !os.IsNotExist(err) {
		t.Fatalf("link not removed: %v", err)
	}
}

func TestPrepareExtraDirsExplicitTargetAndAdopt(t *testing.T) {
	source := t.TempDir()
	work := t.TempDir()
	extras := []runner.ExtraDir{{Source: source, Target: filepath.Join("ctx", "skills")}}
	links, err := prepareExtraDirs(work, extras)
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0] != filepath.Join(work, "ctx", "skills") {
		t.Fatalf("links = %v", links)
	}
	// A second prepare adopts the identical link without owning it.
	again, err := prepareExtraDirs(work, extras)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("adopted link must not be re-owned: %v", again)
	}
}

func TestPrepareExtraDirsConflict(t *testing.T) {
	source := t.TempDir()
	work := t.TempDir()
	target := filepath.Join(work, ".claude", "skills")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: source, Target: filepath.Join(".claude", "skills")}})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestPrepareExtraDirsRejectsEscapes(t *testing.T) {
	source := t.TempDir()
	work := t.TempDir()
	for _, target := range []string{"/abs/path", filepath.Join("..", "outside")} {
		if _, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: source, Target: target}}); err == nil {
			t.Fatalf("target %q must be rejected", target)
		}
	}
	if _, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: filepath.Join(source, "missing")}}); err == nil {
		t.Fatal("missing source must be rejected")
	}
}

func TestPrepareExtraDirsRollsBackOnFailure(t *testing.T) {
	sourceA := t.TempDir()
	work := t.TempDir()
	first := filepath.Join(work, ".claude", filepath.Base(sourceA))
	_, err := prepareExtraDirs(work, []runner.ExtraDir{
		{Source: sourceA},
		{Source: filepath.Join(sourceA, "missing")}, // fails after the first link
	})
	if err == nil {
		t.Fatal("expected failure on second extra dir")
	}
	if _, statErr := os.Lstat(first); !os.IsNotExist(statErr) {
		t.Fatalf("first link must be rolled back: %v", statErr)
	}
}
