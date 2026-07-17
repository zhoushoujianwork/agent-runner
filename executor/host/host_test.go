package host

import (
	"slices"
	"testing"
)

func TestEnvironmentStripsClaudeCodeAndAppliesOverrides(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("KEEP_ME", "yes")
	env := New().environment(map[string]string{"KEEP_ME": "override", "NEW_KEY": "new", "CLAUDECODE": "still-blocked"})
	if slices.Contains(env, "CLAUDECODE=1") {
		t.Fatalf("CLAUDECODE was inherited: %v", env)
	}
	if slices.Contains(env, "CLAUDECODE=still-blocked") {
		t.Fatalf("CLAUDECODE override was not stripped: %v", env)
	}
	if !slices.Contains(env, "KEEP_ME=override") || !slices.Contains(env, "NEW_KEY=new") {
		t.Fatalf("overrides missing: %v", env)
	}
}
