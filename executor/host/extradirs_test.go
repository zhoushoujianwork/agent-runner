package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/agent-runner/runner"
)

// contextRoot builds a fake project with .claude and .agent convention dirs.
func contextRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{
		".claude/skills/review/SKILL.md",
		".claude/commands/deploy.md",
		".agent/agents/planner.md",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPrepareExtraDirsDiscovery(t *testing.T) {
	root := contextRoot(t)
	work := t.TempDir()
	links, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: root}})
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		filepath.Join(work, ".claude", "skills", "review"):      filepath.Join(root, ".claude", "skills", "review"),
		filepath.Join(work, ".claude", "commands", "deploy.md"): filepath.Join(root, ".claude", "commands", "deploy.md"),
		filepath.Join(work, ".agent", "agents", "planner.md"):   filepath.Join(root, ".agent", "agents", "planner.md"),
	}
	if len(links) != len(expected) {
		t.Fatalf("links = %v, want %d entries", links, len(expected))
	}
	for target, source := range expected {
		dest, err := os.Readlink(target)
		if err != nil {
			t.Fatalf("missing link %s: %v", target, err)
		}
		if dest != source {
			t.Fatalf("link %s -> %s, want %s", target, dest, source)
		}
	}
	removeLinks(links)
	for target := range expected {
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("link %s not removed: %v", target, err)
		}
	}
}

func TestPrepareExtraDirsDiscoveryLocalWinsAndAdopts(t *testing.T) {
	root := contextRoot(t)
	work := t.TempDir()
	// The workspace already has its own "review" skill: local wins, skipped.
	local := filepath.Join(work, ".claude", "skills", "review")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	links, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: root}})
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(local); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("local entry must be untouched: %v %v", info, err)
	}
	for _, link := range links {
		if link == local {
			t.Fatal("skipped conflict must not be owned")
		}
	}
	// A second prepare adopts everything already linked: owns nothing new.
	again, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("adopted links must not be re-owned: %v", again)
	}
}

func TestPrepareExtraDirsKeep(t *testing.T) {
	root := contextRoot(t)
	work := t.TempDir()
	links, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: root, Keep: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 0 {
		t.Fatalf("Keep links must not be owned for removal: %v", links)
	}
	if _, err := os.Lstat(filepath.Join(work, ".claude", "skills", "review")); err != nil {
		t.Fatalf("Keep link missing: %v", err)
	}
}

func TestPrepareExtraDirsDiscoveryNoConventions(t *testing.T) {
	work := t.TempDir()
	links, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: t.TempDir()}})
	if err != nil || len(links) != 0 {
		t.Fatalf("source without conventions must be a silent no-op: %v %v", links, err)
	}
}

func TestPrepareExtraDirsExactMode(t *testing.T) {
	source := t.TempDir()
	work := t.TempDir()
	target := filepath.Join("ctx", "skills")
	links, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: source, Target: target}})
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0] != filepath.Join(work, target) {
		t.Fatalf("links = %v", links)
	}
	// Exact mode errors on conflicts instead of skipping.
	if err := os.Remove(links[0]); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(work, target), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: source, Target: target}}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestPrepareExtraDirsRejectsEscapesAndMissing(t *testing.T) {
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

func TestPrepareExtraDirsSweepsDanglingLinks(t *testing.T) {
	root := contextRoot(t)
	work := t.TempDir()

	// An unrelated dead link left by a prior run: its source is gone.
	danglingDir := filepath.Join(work, ".claude", "skills")
	if err := os.MkdirAll(danglingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(danglingDir, "orphan")
	if err := os.Symlink(filepath.Join(work, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	// A valid external link pointing elsewhere must survive untouched.
	elsewhere := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(work, ".agent", "commands")
	if err := os.MkdirAll(external, 0o755); err != nil {
		t.Fatal(err)
	}
	extLink := filepath.Join(external, "keepme")
	if err := os.Symlink(elsewhere, extLink); err != nil {
		t.Fatal(err)
	}
	// A real file must never be swept.
	realFile := filepath.Join(work, ".claude", "agents", "local.md")
	if err := os.MkdirAll(filepath.Dir(realFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: root}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dangling); !os.IsNotExist(err) {
		t.Fatalf("dangling link must be swept: %v", err)
	}
	if _, err := os.Lstat(extLink); err != nil {
		t.Fatalf("valid external link must survive: %v", err)
	}
	if _, err := os.Lstat(realFile); err != nil {
		t.Fatalf("real file must survive: %v", err)
	}
}

func TestPrepareExtraDirsReplacesDanglingTargetDiscovery(t *testing.T) {
	root := contextRoot(t)
	work := t.TempDir()
	// The target position holds a dead link pointing at a since-moved source.
	target := filepath.Join(work, ".claude", "skills", "review")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(work, "moved-away"), target); err != nil {
		t.Fatal(err)
	}
	links, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: root}})
	if err != nil {
		t.Fatal(err)
	}
	// The dead link must be replaced by a fresh, owned link, not skipped.
	dest, err := os.Readlink(target)
	if err != nil {
		t.Fatalf("target should be relinked: %v", err)
	}
	if want := filepath.Join(root, ".claude", "skills", "review"); dest != want {
		t.Fatalf("target -> %s, want %s", dest, want)
	}
	var owned bool
	for _, l := range links {
		if l == target {
			owned = true
		}
	}
	if !owned {
		t.Fatalf("replaced dangling target must be owned: %v", links)
	}
}

func TestPrepareExtraDirsReplacesDanglingTargetExact(t *testing.T) {
	source := t.TempDir()
	work := t.TempDir()
	rel := filepath.Join("ctx", "skills")
	target := filepath.Join(work, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	// A dead link at the exact target must be replaced, not raise a conflict.
	if err := os.Symlink(filepath.Join(work, "vanished"), target); err != nil {
		t.Fatal(err)
	}
	links, err := prepareExtraDirs(work, []runner.ExtraDir{{Source: source, Target: rel}})
	if err != nil {
		t.Fatalf("dangling target must be replaced, not error: %v", err)
	}
	if len(links) != 1 || links[0] != target {
		t.Fatalf("links = %v", links)
	}
	dest, err := os.Readlink(target)
	if err != nil || dest != source {
		t.Fatalf("target -> %s (%v), want %s", dest, err, source)
	}
}

func TestPrepareExtraDirsRollsBackOnFailure(t *testing.T) {
	root := contextRoot(t)
	work := t.TempDir()
	first := filepath.Join(work, ".claude", "skills", "review")
	_, err := prepareExtraDirs(work, []runner.ExtraDir{
		{Source: root},
		{Source: filepath.Join(root, "missing")}, // fails after the first root linked
	})
	if err == nil {
		t.Fatal("expected failure on second extra dir")
	}
	if _, statErr := os.Lstat(first); !os.IsNotExist(statErr) {
		t.Fatalf("links from the first root must be rolled back: %v", statErr)
	}
}
