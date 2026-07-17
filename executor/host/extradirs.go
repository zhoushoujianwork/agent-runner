package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhoushoujianwork/agent-runner/runner"
)

// prepareExtraDirs symlinks each extra dir's source into the working
// directory before the process starts and returns the links it created (for
// removal when the process exits). A target that is already a symlink to the
// same source is adopted without ownership; any other existing target is a
// conflict. On error, links created so far are rolled back.
func prepareExtraDirs(workDir string, extras []runner.ExtraDir) ([]string, error) {
	if len(extras) == 0 {
		return nil, nil
	}
	base := workDir
	if base == "" {
		base = "."
	}
	absBase, err := filepath.Abs(base)
	if err != nil {
		return nil, fmt.Errorf("resolve working directory: %w", err)
	}

	var created []string
	fail := func(err error) ([]string, error) {
		removeLinks(created)
		return nil, err
	}
	for _, extra := range extras {
		if strings.TrimSpace(extra.Source) == "" {
			return fail(errors.New("empty source"))
		}
		source, err := filepath.Abs(extra.Source)
		if err != nil {
			return fail(fmt.Errorf("resolve source %q: %w", extra.Source, err))
		}
		info, err := os.Stat(source)
		if err != nil {
			return fail(fmt.Errorf("source %q: %w", extra.Source, err))
		}
		if !info.IsDir() {
			return fail(fmt.Errorf("source %q is not a directory", extra.Source))
		}

		target := extra.Target
		if target == "" {
			target = filepath.Join(".claude", filepath.Base(source))
		}
		if filepath.IsAbs(target) {
			return fail(fmt.Errorf("target %q must be relative to the working directory", extra.Target))
		}
		absTarget := filepath.Join(absBase, target)
		if rel, err := filepath.Rel(absBase, absTarget); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fail(fmt.Errorf("target %q escapes the working directory", extra.Target))
		}

		if existing, err := os.Lstat(absTarget); err == nil {
			if existing.Mode()&os.ModeSymlink != 0 {
				if dest, err := os.Readlink(absTarget); err == nil && dest == source {
					continue // already linked by someone else; adopt, do not own
				}
			}
			return fail(fmt.Errorf("target %q already exists", target))
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(fmt.Errorf("inspect target %q: %w", target, err))
		}

		if err := os.MkdirAll(filepath.Dir(absTarget), 0o755); err != nil {
			return fail(fmt.Errorf("create target parent for %q: %w", target, err))
		}
		if err := os.Symlink(source, absTarget); err != nil {
			return fail(fmt.Errorf("link %q -> %q: %w", target, extra.Source, err))
		}
		created = append(created, absTarget)
	}
	return created, nil
}

func removeLinks(links []string) {
	for _, link := range links {
		_ = os.Remove(link)
	}
}
