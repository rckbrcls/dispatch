package verify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"dispatch/internal/sandbox"
)

const (
	checkDiff      = "diff-check"
	checkConflicts = "conflicts"
)

var (
	testFunc = regexp.MustCompile(`^func\s+(Test\w+)\s*\(`)
	testMeth = regexp.MustCompile(`^func\s+\([^)]+\)\s+(Test\w+)\s*\(`)
)

var lockfiles = map[string]bool{
	"go.sum": true, "package-lock.json": true, "yarn.lock": true,
	"pnpm-lock.yaml": true, "bun.lock": true, "bun.lockb": true,
	"npm-shrinkwrap.json": true, "Cargo.lock": true, "Gemfile.lock": true,
	"poetry.lock": true, "Pipfile.lock": true, "uv.lock": true,
	"composer.lock": true, "Package.resolved": true, "packages.lock.json": true,
	"flake.lock": true, "pubspec.lock": true, "mix.lock": true,
	"Podfile.lock": true, "gradle.lockfile": true,
}

// cheapChecks compares BaseSHA to HEAD. Warnings never fail the run.
// diff --check and conflict markers in added lines do.
func cheapChecks(ctx context.Context, worktree, base string) (warnings []string, fails []CheckResult, err error) {
	diffOut, err := gitDiff(ctx, worktree, base, "--check")
	if err != nil && !diffCheckFailed(err, diffOut) {
		return nil, nil, fmt.Errorf("git diff --check: %w", err)
	}
	if strings.TrimSpace(diffOut) != "" && (err == nil || diffCheckFailed(err, diffOut)) {
		var regs []string
		for _, line := range strings.Split(diffOut, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			regs = append(regs, line)
			if len(regs) == 30 {
				break
			}
		}
		if len(regs) > 0 {
			fails = append(fails, CheckResult{
				Name: checkDiff, Status: CheckFailed, ExitCode: 2, Regressions: regs,
			})
		}
	}

	patch, err := gitDiff(ctx, worktree, base, "-U0")
	if err != nil {
		return nil, nil, fmt.Errorf("git diff: %w", err)
	}
	conflicts, removed := scanPatch(patch)
	if len(conflicts) > 0 {
		fails = append(fails, CheckResult{
			Name: checkConflicts, Status: CheckFailed, Regressions: conflicts,
		})
	}
	warnings = append(warnings, removed...)

	names, err := gitDiff(ctx, worktree, base, "--name-only")
	if err != nil {
		return nil, nil, fmt.Errorf("git diff --name-only: %w", err)
	}
	var extra []string
	for _, p := range strings.Split(names, "\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		baseName := p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			baseName = p[i+1:]
		}
		switch {
		case strings.HasPrefix(p, ".github/workflows/"):
			extra = append(extra, "workflow changed: "+p)
		case lockfiles[baseName]:
			extra = append(extra, "lockfile changed: "+p)
		}
	}
	sort.Strings(extra)
	warnings = append(warnings, extra...)
	return warnings, fails, nil
}

func gitDiff(ctx context.Context, dir, base, flag string) (string, error) {
	args := []string{"-C", dir, "-c", "core.whitespace=blank-at-eol,blank-at-eof,space-before-tab", "diff", flag, base, "HEAD", "--"}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = sandbox.Environ()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// diffCheckFailed reports a whitespace or conflict finding rather than a
// git failure. git diff --check exits 2 when it finds problems.
func diffCheckFailed(err error, out string) bool {
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	if ee.ExitCode() == 2 {
		return true
	}
	trimmed := strings.TrimSpace(out)
	return ee.ExitCode() == 1 && trimmed != "" && !strings.HasPrefix(trimmed, "fatal:") && !strings.HasPrefix(trimmed, "error:")
}

func scanPatch(diff string) (conflicts, removed []string) {
	var file string
	gone := map[string]string{}
	added := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			file = diffPath(line)
			continue
		}
		if file == "" || strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || strings.HasPrefix(line, "@@") {
			continue
		}
		add := strings.HasPrefix(line, "+")
		del := strings.HasPrefix(line, "-")
		if !add && !del {
			continue
		}
		body := line[1:]
		if add && (strings.Contains(body, "<<<<<<<") || strings.Contains(body, ">>>>>>>")) {
			conflicts = append(conflicts, file+": "+strings.TrimSpace(body))
			if len(conflicts) > 30 {
				conflicts = conflicts[:30]
			}
		}
		if !strings.HasSuffix(file, "_test.go") {
			continue
		}
		name := testName(body)
		if name == "" {
			continue
		}
		if add {
			added[name] = true
		} else if _, ok := gone[name]; !ok {
			gone[name] = file
		}
	}
	for name, path := range gone {
		if added[name] {
			continue
		}
		removed = append(removed, fmt.Sprintf("removed test %s in %s", name, path))
	}
	sort.Strings(removed)
	sort.Strings(conflicts)
	if len(removed) > 30 {
		removed = removed[:30]
	}
	return conflicts, removed
}

func diffPath(line string) string {
	i := strings.LastIndex(line, " b/")
	if i < 0 {
		return ""
	}
	return line[i+3:]
}

func testName(body string) string {
	body = strings.TrimSpace(body)
	if m := testMeth.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	if m := testFunc.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}
