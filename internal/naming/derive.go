package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// DeriveSources are the values accepted by `--from` / the `from:` config key.
var DeriveSources = []string{"port", "dir", "repo", "branch", "worktree"}

var multiHyphen = regexp.MustCompile(`-{2,}`)

// Sanitize turns an arbitrary string into a single valid RFC 1123 label:
// lowercased, non-[a-z0-9-] replaced with hyphens, runs collapsed, ends
// trimmed, and length-capped to 63 with a hash suffix (so two distinct
// long names don't collide on a hard truncation). Returns "" if nothing
// usable remains.
func Sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := multiHyphen.ReplaceAllString(b.String(), "-")
	out = strings.Trim(out, "-")
	return truncateLabel(out)
}

// truncateLabel caps a label at 63 chars (the RFC 1123 limit). When it has
// to cut, it appends "-" + the first 6 hex chars of the SHA-256 of the full
// input so long-but-distinct names stay distinct (pattern from portless).
func truncateLabel(s string) string {
	const max = 63
	if len(s) <= max {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	suffix := "-" + hex.EncodeToString(sum[:])[:6] // 7 chars total
	prefix := strings.TrimRight(s[:max-len(suffix)], "-")
	return prefix + suffix
}

// DeriveLabel resolves a `--from` source to a tunnel label, run relative to
// dir (the invocation's cwd). `port` supplies the default/"port" source.
// Every result is sanitized and validated; an unusable source (detached
// HEAD, not a git repo, empty dir name) returns a clear, actionable error.
func DeriveLabel(source string, port int, dir string) (string, error) {
	var label string
	switch source {
	case "", "port":
		return LabelFromPort(port), nil
	case "dir":
		label = Sanitize(filepath.Base(dir))
		if label == "" {
			return "", fmt.Errorf("--from dir: can't derive a label from directory %q", dir)
		}
	case "repo":
		top, err := gitOutput(dir, "rev-parse", "--show-toplevel")
		if err != nil {
			return "", fmt.Errorf("--from repo: not inside a git repository (run from a repo, or use --as)")
		}
		label = Sanitize(filepath.Base(top))
		if label == "" {
			return "", fmt.Errorf("--from repo: repo name %q has no usable label characters", filepath.Base(top))
		}
	case "branch":
		label, err := branchLabel(dir)
		if err != nil {
			return "", err
		}
		if err := ValidateLabel(label); err != nil {
			return "", fmt.Errorf("--from branch: derived label %q is invalid: %w", label, err)
		}
		return label, nil
	case "worktree":
		return worktreeLabel(dir)
	default:
		return "", fmt.Errorf("unknown --from source %q (want one of: %s)", source, strings.Join(DeriveSources, ", "))
	}
	if err := ValidateLabel(label); err != nil {
		return "", fmt.Errorf("--from %s: derived label %q is invalid: %w", source, label, err)
	}
	return label, nil
}

// branchLabel returns the sanitized current branch. Uses symbolic-ref so it
// works on an unborn branch (fresh repo, no commits) and fails cleanly only
// on detached HEAD.
func branchLabel(dir string) (string, error) {
	br, err := gitOutput(dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		if !inGitRepo(dir) {
			return "", fmt.Errorf("--from branch: not inside a git repository (run from a repo, or use --as)")
		}
		return "", fmt.Errorf("--from branch: detached HEAD has no branch name — check out a branch or use --as")
	}
	label := Sanitize(br)
	if label == "" {
		return "", fmt.Errorf("--from branch: branch %q has no usable label characters", br)
	}
	return label, nil
}

func inGitRepo(dir string) bool {
	_, err := gitOutput(dir, "rev-parse", "--git-dir")
	return err == nil
}

// worktreeLabel composes <branch>-<repo> as a single label when in a *linked*
// git worktree on a non-default branch (beamd is one DNS label deep, so the
// branch can't be a separate subdomain like in portless). In the primary
// checkout — or on main/master — it falls back to the plain repo name.
func worktreeLabel(dir string) (string, error) {
	top, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("--from worktree: not inside a git repository (run from a repo, or use --as)")
	}
	repo := Sanitize(filepath.Base(top))

	gitDir, errA := gitOutput(dir, "rev-parse", "--absolute-git-dir")
	commonDir, errB := gitOutput(dir, "rev-parse", "--git-common-dir")
	linked := errA == nil && errB == nil && absClean(gitDir) != absClean(commonDir)

	if linked {
		if br, err := gitOutput(dir, "symbolic-ref", "--short", "HEAD"); err == nil &&
			br != "" && br != "main" && br != "master" {
			if combined := Sanitize(br + "-" + filepath.Base(top)); combined != "" {
				if err := ValidateLabel(combined); err == nil {
					return combined, nil
				}
			}
		}
	}
	if repo == "" {
		return "", fmt.Errorf("--from worktree: repo name %q has no usable label characters", filepath.Base(top))
	}
	if err := ValidateLabel(repo); err != nil {
		return "", fmt.Errorf("--from worktree: derived label %q is invalid: %w", repo, err)
	}
	return repo, nil
}

func absClean(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// gitOutput runs `git -C dir <args...>` and returns trimmed stdout.
func gitOutput(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
