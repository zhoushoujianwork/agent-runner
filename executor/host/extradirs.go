package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhoushoujianwork/agent-runner/runner"
)

// Agent-context conventions: directories the agent CLI discovers in its
// working directory, and the content dirs inside them whose entries are
// merged one link at a time.
var (
	conventionDirs = []string{".claude", ".agent"}
	contentDirs    = []string{"skills", "agents", "commands"}
)

// prepareExtraDirs wires each extra dir into the working directory before the
// process starts and returns the links this run created and must remove when
// the process exits (Keep and adopted links are never returned).
//
// Discovery mode (Target empty): Source is a context root; the entries of
// Source/{.claude,.agent}/{skills,agents,commands} are linked into the same
// relative location under the working directory. Existing local entries win
// silently; identical links are adopted.
//
// Exact mode (Target set): Source itself is linked at Target; an existing
// Target that is not an identical link is an error. On error, links created
// so far are rolled back.
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

	sweepDanglingLinks(absBase)

	prep := &linkPrep{base: absBase}
	for _, extra := range extras {
		if strings.TrimSpace(extra.Source) == "" {
			return prep.fail(errors.New("empty source"))
		}
		source, err := filepath.Abs(extra.Source)
		if err != nil {
			return prep.fail(fmt.Errorf("resolve source %q: %w", extra.Source, err))
		}
		info, err := os.Stat(source)
		if err != nil {
			return prep.fail(fmt.Errorf("source %q: %w", extra.Source, err))
		}
		if !info.IsDir() {
			return prep.fail(fmt.Errorf("source %q is not a directory", extra.Source))
		}

		if extra.Target != "" {
			err = prep.linkExact(source, extra)
		} else {
			err = prep.discover(source, extra)
		}
		if err != nil {
			return prep.fail(err)
		}
	}
	return prep.owned, nil
}

type linkPrep struct {
	base    string
	owned   []string // links to remove on process exit
	created []string // every link this prep created, for rollback
}

func (p *linkPrep) fail(err error) ([]string, error) {
	removeLinks(p.created)
	return nil, err
}

// discover merges the convention dirs of one context root into the base dir,
// one link per content entry.
func (p *linkPrep) discover(source string, extra runner.ExtraDir) error {
	for _, convention := range conventionDirs {
		for _, content := range contentDirs {
			sourceDir := filepath.Join(source, convention, content)
			if info, err := os.Stat(sourceDir); err != nil || !info.IsDir() {
				continue
			}
			entries, err := os.ReadDir(sourceDir)
			if err != nil {
				return fmt.Errorf("read %q: %w", sourceDir, err)
			}
			for _, entry := range entries {
				entrySource := filepath.Join(sourceDir, entry.Name())
				entryTarget := filepath.Join(p.base, convention, content, entry.Name())
				if err := p.link(entrySource, entryTarget, extra.Keep, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// linkExact links the source verbatim at the caller-chosen target.
func (p *linkPrep) linkExact(source string, extra runner.ExtraDir) error {
	if filepath.IsAbs(extra.Target) {
		return fmt.Errorf("target %q must be relative to the working directory", extra.Target)
	}
	target := filepath.Join(p.base, extra.Target)
	if rel, err := filepath.Rel(p.base, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("target %q escapes the working directory", extra.Target)
	}
	return p.link(source, target, extra.Keep, false)
}

// link creates one symlink. A dangling link occupying the target is removed
// first (it deserves neither the merge-mode skip nor the exact-mode error).
// An existing identical link is adopted without ownership; any other existing
// target is skipped in merge mode (the local entry wins) and an error in exact
// mode.
func (p *linkPrep) link(source, target string, keep, skipConflicts bool) error {
	if existing, err := os.Lstat(target); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 {
			// A dangling link is dead weight: clear it before the adopt/skip
			// checks so it never blocks a fresh mount.
			if _, statErr := os.Stat(target); errors.Is(statErr, os.ErrNotExist) {
				if err := os.Remove(target); err != nil {
					return fmt.Errorf("remove dangling target %q: %w", target, err)
				}
			} else if dest, err := os.Readlink(target); err == nil && dest == source {
				return nil // adopted; not ours to remove
			} else {
				if skipConflicts {
					return nil // local entry wins
				}
				return fmt.Errorf("target %q already exists", target)
			}
		} else if skipConflicts {
			return nil // local entry wins
		} else {
			return fmt.Errorf("target %q already exists", target)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect target %q: %w", target, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create parent for %q: %w", target, err)
	}
	if err := os.Symlink(source, target); err != nil {
		return fmt.Errorf("link %q -> %q: %w", target, source, err)
	}
	p.created = append(p.created, target)
	if !keep {
		p.owned = append(p.owned, target)
	}
	return nil
}

// sweepDanglingLinks removes dead symlinks left in the convention dirs by a
// prior run (host killed, power loss, or a Keep link whose source moved).
// Only the top-level entries of <base>/{.claude,.agent}/{skills,agents,commands}
// are inspected; real files/dirs and links pointing at existing targets stay.
// Best-effort: permission errors and the like never block startup.
func sweepDanglingLinks(base string) {
	for _, convention := range conventionDirs {
		for _, content := range contentDirs {
			dir := filepath.Join(base, convention, content)
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				target := filepath.Join(dir, entry.Name())
				info, err := os.Lstat(target)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					continue // real file/dir or vanished; leave it
				}
				if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
					_ = os.Remove(target) // dangling; best-effort
				}
			}
		}
	}
}

func removeLinks(links []string) {
	for _, link := range links {
		_ = os.Remove(link)
	}
}
