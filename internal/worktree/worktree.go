// Package worktree isolates write runs in a git worktree on a dedicated
// branch so parallel runs never share a working tree.
package worktree

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// gitRaw returns stdout untouched (no trimming, no stderr mixed in), which
// -z output needs: a porcelain line can start with a space and paths can hold
// any byte except NUL.
func gitRaw(dir string, args ...string) (string, int, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return string(out), 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode(), nil
	}
	return string(out), -1, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
}

func gitExit(dir string, args ...string) (string, int, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err == nil {
		return s, 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return s, ee.ExitCode(), nil
	}
	return s, -1, err
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

// NumStat counts changes from base to the working tree (committed and not).
// linesKnown is false when a binary file hides the line counts; the file
// count is still valid. An error leaves every count unknown to the caller.
func NumStat(dir, base string) (files, insertions, deletions int, linesKnown bool, err error) {
	if base == "" {
		return 0, 0, 0, false, fmt.Errorf("no base")
	}
	out, err := git(dir, "diff", "--numstat", base)
	if err != nil {
		return 0, 0, 0, false, err
	}
	linesKnown = true
	if out == "" {
		return 0, 0, 0, true, nil
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		files++
		if parts[0] == "-" || parts[1] == "-" {
			linesKnown = false
			continue
		}
		add, e1 := strconv.Atoi(parts[0])
		sub, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil {
			linesKnown = false
			continue
		}
		insertions += add
		deletions += sub
	}
	return files, insertions, deletions, linesKnown, nil
}

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

func parseGitVersion(s string) (major, minor int, ok bool) {
	idx := strings.Index(s, "git version ")
	if idx != -1 {
		s = s[idx+len("git version "):]
	}
	parts := strings.Split(strings.TrimSpace(s), " ")
	if len(parts) == 0 {
		return 0, 0, false
	}
	nums := strings.Split(parts[0], ".")
	if len(nums) < 2 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(nums[0])
	min, err2 := strconv.Atoi(nums[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

func defaultSupportsMergeTree(root string) bool {
	out, err := git(root, "version")
	if err != nil {
		return false
	}
	maj, min, ok := parseGitVersion(out)
	if !ok {
		return false
	}
	return maj > 2 || (maj == 2 && min >= 38)
}

// SupportsMergeTree reports whether git supports merge-tree --write-tree (git >= 2.38).
// Injectable for testing older git versions.
var SupportsMergeTree = defaultSupportsMergeTree

// MergeTreeResult represents the outcome of a pre-merge merge-tree check.
type MergeTreeResult struct {
	Clean     bool
	Conflicts []string
	TreeOID   string
}

// parseConflicts reads `git merge-tree --write-tree --name-only -z` output:
// "<tree>\0<path>\0<path>\0\0<messages>". Only the NUL-terminated section
// before the empty field holds conflicted paths; the free-form messages after
// it are not parsed, so odd file names cannot be misread.
func parseConflicts(out string) []string {
	fields := strings.Split(out, "\x00")
	seen := make(map[string]bool)
	var list []string
	for _, f := range fields[1:] {
		if f == "" {
			break
		}
		if !seen[f] {
			seen[f] = true
			list = append(list, f)
		}
	}
	sort.Strings(list)
	return list
}

// MergeTree runs git merge-tree --write-tree to check if branch can merge cleanly into target.
func MergeTree(root, target, branch string) (MergeTreeResult, error) {
	out, code, err := gitRaw(root, "merge-tree", "--write-tree", "--name-only", "-z", target, branch)
	if err != nil {
		return MergeTreeResult{}, err
	}
	switch code {
	case 0:
		return MergeTreeResult{Clean: true, TreeOID: strings.TrimRight(out, "\x00\n")}, nil
	case 1:
		return MergeTreeResult{Clean: false, Conflicts: parseConflicts(out)}, nil
	}
	return MergeTreeResult{}, fmt.Errorf("git merge-tree exited with code %d: %s", code, strings.TrimSpace(out))
}

// CurrentBranch returns the currently checked out branch in root, or "HEAD" if detached.
func CurrentBranch(root string) (string, error) {
	b, err := git(root, "symbolic-ref", "--short", "HEAD")
	if err == nil && b != "" {
		return b, nil
	}
	b, err = git(root, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil && b != "" {
		return b, nil
	}
	return "HEAD", nil
}

// LockTimeout is the maximum duration to wait for a merge lock.
// Tests can lower this for fast concurrency tests.
var LockTimeout = 3 * time.Second

func sanitizeBranch(branch string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return r.Replace(branch)
}

// LockMerge acquires an exclusive lock for merging into targetBranch in repo.
// It serializes merges using flock and records the holder's PID for stale detection.
// If the lock is held, it waits up to LockTimeout.
func LockMerge(repo, targetBranch string) (unlock func(), err error) {
	if targetBranch == "" {
		targetBranch, _ = CurrentBranch(repo)
	}
	dispatchDir := filepath.Join(repo, ".dispatch")
	if err := os.MkdirAll(dispatchDir, 0o755); err != nil {
		return nil, err
	}
	_ = Exclude(repo, ".dispatch/")

	lockPath := filepath.Join(dispatchDir, fmt.Sprintf("merge-%s.lock", sanitizeBranch(targetBranch)))
	deadline := time.Now().Add(LockTimeout)

	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}

		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = f.Truncate(0)
			_, _ = f.Seek(0, 0)
			fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Sync()

			unlocked := false
			unlock = func() {
				if unlocked {
					return
				}
				unlocked = true
				_ = f.Truncate(0)
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				// The file stays: unlinking while waiters hold the old inode would
				// let two processes lock different files at once.
				_ = f.Close()
			}
			return unlock, nil
		}

		var holderPID int
		b, _ := io.ReadAll(f)
		f.Close()
		pidStr := strings.TrimSpace(string(b))
		if pidStr != "" {
			holderPID, _ = strconv.Atoi(pidStr)
		}

		if time.Now().After(deadline) {
			if holderPID > 0 {
				return nil, fmt.Errorf("merge lock for branch %q in %s is held by pid %d; timeout waiting for lock", targetBranch, repo, holderPID)
			}
			return nil, fmt.Errorf("merge lock for branch %q in %s is held by another process; timeout waiting for lock", targetBranch, repo)
		}

		time.Sleep(25 * time.Millisecond)
	}
}

// UncommittedOverlaps returns files in root that have uncommitted changes
// AND are touched by the branch relative to HEAD.
func UncommittedOverlaps(root, branch string) ([]string, error) {
	uncommitted := make(map[string]bool)
	if out, code, err := gitRaw(root, "diff", "--name-only", "--no-renames", "-z", "HEAD"); err == nil && code == 0 {
		for _, f := range strings.Split(out, "\x00") {
			if f != "" {
				uncommitted[f] = true
			}
		}
	}
	// Porcelain v1 -z entries are "XY <path>" and, for renames/copies, are
	// followed by one extra "<orig path>" field. Do not trim: " M x" is valid.
	if out, code, err := gitRaw(root, "status", "--porcelain=v1", "-z"); err == nil && code == 0 {
		fields := strings.Split(out, "\x00")
		for i := 0; i < len(fields); i++ {
			e := fields[i]
			if len(e) < 4 {
				continue
			}
			uncommitted[e[3:]] = true
			if e[0] == 'R' || e[0] == 'C' || e[1] == 'R' || e[1] == 'C' {
				i++
				if i < len(fields) && fields[i] != "" {
					uncommitted[fields[i]] = true
				}
			}
		}
	}
	if len(uncommitted) == 0 {
		return nil, nil
	}

	out, code, err := gitRaw(root, "diff", "--name-only", "--no-renames", "-z", "HEAD..."+branch)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("git diff HEAD...%s exited with code %d", branch, code)
	}
	seen := make(map[string]bool)
	var overlapping []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" && uncommitted[f] && !seen[f] {
			seen[f] = true
			overlapping = append(overlapping, f)
		}
	}
	sort.Strings(overlapping)
	return overlapping, nil
}
