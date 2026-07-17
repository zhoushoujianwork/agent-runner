package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	runner "github.com/zhoushoujianwork/agent-runner/runner"
)

// placeExtraDirs links each declared context source into dir before the agent
// process starts. It returns the absolute paths of the links this call created
// (idempotently adopted, pre-existing links are excluded) so the caller can
// remove exactly what it made on process exit. On any failure it rolls back the
// links it created and returns the error.
func placeExtraDirs(dir string, extras []runner.ExtraDir) ([]string, error) {
	if len(extras) == 0 {
		return nil, nil
	}
	if dir == "" {
		return nil, fmt.Errorf("extra dirs require a working directory")
	}
	baseAbs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve work dir: %w", err)
	}

	var created []string
	for _, extra := range extras {
		linkPath, err := placeExtraDir(baseAbs, extra)
		if err != nil {
			removeLinks(created)
			return nil, err
		}
		if linkPath != "" {
			created = append(created, linkPath)
		}
	}
	return created, nil
}

// placeExtraDir validates and links one ExtraDir. It returns the absolute link
// path when this call created the link, or "" when an identical link already
// existed (idempotent adoption: not owned by this process, so not cleaned up).
func placeExtraDir(baseAbs string, extra runner.ExtraDir) (string, error) {
	if strings.TrimSpace(extra.Source) == "" {
		return "", fmt.Errorf("extra dir: empty source")
	}
	sourceAbs, err := filepath.Abs(extra.Source)
	if err != nil {
		return "", fmt.Errorf("extra dir %q: resolve source: %w", extra.Source, err)
	}
	info, err := os.Stat(sourceAbs)
	if err != nil {
		return "", fmt.Errorf("extra dir source %q: %w", extra.Source, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("extra dir source %q is not a directory", extra.Source)
	}

	target := extra.Target
	if strings.TrimSpace(target) == "" {
		target = filepath.Join(".claude", filepath.Base(sourceAbs))
	}
	if filepath.IsAbs(target) {
		return "", fmt.Errorf("extra dir target %q must be relative to the work dir", target)
	}
	cleanTarget := filepath.Clean(target)
	if cleanTarget == ".." || strings.HasPrefix(cleanTarget, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("extra dir target %q escapes the work dir", target)
	}
	linkPath := filepath.Join(baseAbs, cleanTarget)
	rel, err := filepath.Rel(baseAbs, linkPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("extra dir target %q escapes the work dir", target)
	}
	return linkExtraDir(linkPath, sourceAbs, target)
}

// linkExtraDir creates the symlink at linkPath pointing to sourceAbs. When
// linkPath is already a symlink resolving to the same source it is adopted
// idempotently (returns "" so the caller does not remove it). Any other
// existing entry is a conflict.
func linkExtraDir(linkPath, sourceAbs, target string) (string, error) {
	if existing, err := os.Lstat(linkPath); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 {
			dest, readErr := os.Readlink(linkPath)
			if readErr == nil {
				if !filepath.IsAbs(dest) {
					dest = filepath.Join(filepath.Dir(linkPath), dest)
				}
				if resolved, evalErr := filepath.EvalSymlinks(dest); evalErr == nil {
					if sourceResolved, srcErr := filepath.EvalSymlinks(sourceAbs); srcErr == nil && resolved == sourceResolved {
						return "", nil
					}
				}
			}
		}
		return "", &runner.RunError{Kind: runner.ErrorStart, Op: "extra dir link", Err: fmt.Errorf("target %q already exists", target)}
	}

	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return "", &runner.RunError{Kind: runner.ErrorStart, Op: "extra dir link", Err: fmt.Errorf("target %q: %w", target, err)}
	}
	if err := os.Symlink(sourceAbs, linkPath); err != nil {
		return "", &runner.RunError{Kind: runner.ErrorStart, Op: "extra dir link", Err: fmt.Errorf("target %q: %w", target, err)}
	}
	return linkPath, nil
}

// removeLinks removes the symlinks this process created, best effort.
func removeLinks(links []string) {
	for _, link := range links {
		if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink != 0 {
			_ = os.Remove(link)
		}
	}
}
