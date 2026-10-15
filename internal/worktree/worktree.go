// Package worktree isolates write runs in a git worktree on a dedicated
// branch so parallel runs never share a working tree.
package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

// Root returns the repo top-level containing dir, or "" when dir is not in
// a git work tree.
func Root(dir string) string {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return out
}

// HasCommits reports whether HEAD resolves (a worktree needs a commit).
func HasCommits(root string) bool {
	_, err := git(root, "rev-parse", "--verify", "HEAD")
	return err == nil
}

// Dirty reports whether the checkout has uncommitted changes, which a new
// worktree (cut from HEAD) will not include.
func Dirty(root string) bool {
	out, err := git(root, "status", "--porcelain")
	return err == nil && out != ""
}

// Exclude adds pattern to the repo's local .git/info/exclude (untracked,
// never committed) if it is not there yet.
func Exclude(root, pattern string) error {
	p, err := git(root, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	b, _ := os.ReadFile(p)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == pattern {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + pattern + "\n")
	return err
}

// Create adds worktree path on new branch dispatch/<id> from HEAD.
func Create(root, path, id string) (branch string, err error) {
	branch = "dispatch/" + id
	_, err = git(root, "worktree", "add", "-b", branch, path, "HEAD")
	return branch, err
}

// Commit stages and commits everything in the worktree. It reports whether
// a commit was made (false when there was nothing to commit).
func Commit(path, id string) (bool, error) {
	return commit(path, "dispatch run "+id)
}

func commit(path, message string) (bool, error) {
	out, err := git(path, "status", "--porcelain")
	if err != nil || out == "" {
		return false, err
	}
	if _, err := git(path, "add", "-A"); err != nil {
		return false, err
	}
	_, err = git(path, "-c", "user.name=dispatch", "-c", "user.email=dispatch@localhost",
		"commit", "-q", "-m", message)
	return err == nil, err
}

// Merge merges branch into the repo's current branch with a merge commit.
func Merge(root, branch, id string) (string, error) {
	return git(root, "merge", "--no-ff", "-m", "Merge dispatch run "+id, branch)
}

// Remove deletes the worktree and its branch. The branch is deleted with -d
// (merged only) unless force is set.
func Remove(root, path, branch string, force bool) error {
	if !force {
		status, err := git(path, "status", "--porcelain")
		if err != nil {
			return err
		}
		if status != "" {
			return fmt.Errorf("worktree is dirty; use --force to discard changes")
		}
	}
	args := []string{"worktree", "remove", path}
	if force {
		args = append(args, "--force")
	}
	if _, err := git(root, args...); err != nil {
		return err
	}
	del := "-d"
	if force {
		del = "-D"
	}
	_, err := git(root, "branch", del, branch)
	return err
}

// Rel returns dir relative to root ("." when equal or outside).
func Rel(root, dir string) string {
	r, err := filepath.Rel(root, dir)
	if err != nil || strings.HasPrefix(r, "..") {
		return "."
	}
	return r
}

// Head returns the commit used as the base of a new worktree.
func Head(path string) (string, error) { return git(path, "rev-parse", "HEAD") }

// Recover checks the run branch before committing and reporting its diff.
func Recover(path, branch, base, id string) (string, error) {
	current, err := git(path, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	if branch == "" || current != branch {
		return "", fmt.Errorf("worktree is not on run branch %s", branch)
	}
	if base == "" {
		// Older runs predate BaseSHA; use the branch's creation reflog entry.
		base, err = git(path, "rev-parse", branch+"@{0}")
		if err != nil {
			return "", err
		}
		log, err := git(path, "reflog", "show", "--format=%H", branch)
		if err != nil {
			return "", err
		}
		entries := strings.Fields(log)
		if len(entries) > 0 {
			base = entries[len(entries)-1]
		}
	}
	if _, err := git(path, "rev-parse", "--verify", base+"^{commit}"); err != nil {
		return "", err
	}
	if _, err := commit(path, "dispatch recover "+id); err != nil {
		return "", err
	}
	return git(path, "diff", "--stat", base, "HEAD", "--")
}
