// Package verify runs deterministic checks on a write-run worktree and
// compares failures with the same checks at the run's base commit.
package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const defaultTimeout = 10 * time.Minute

// Status is the verdict for a run. error means the verifier itself failed.
type Status string

const (
	StatusVerified   Status = "verified"
	StatusFailed     Status = "verification_failed"
	StatusUnverified Status = "unverified"
	StatusError      Status = "error"
)

// CheckStatus is the outcome of one check.
type CheckStatus string

const (
	CheckPassed  CheckStatus = "passed"
	CheckFailed  CheckStatus = "failed"
	CheckSkipped CheckStatus = "skipped"
	CheckError   CheckStatus = "error"
	CheckTimeout CheckStatus = "timeout"
)

// Result is the verifier verdict stored on a run.
type Result struct {
	Status         Status        `json:"status"`
	Checks         []CheckResult `json:"checks,omitempty"`
	Warnings       []string      `json:"warnings,omitempty"`
	DurationMs     int64         `json:"durationMs,omitempty"`
	BaselineCached bool          `json:"baselineCached,omitempty"`
}

// CheckResult is one configured check or a cheap diff check that failed.
type CheckResult struct {
	Name        string      `json:"name"`
	Status      CheckStatus `json:"status"`
	ExitCode    int         `json:"exitCode,omitempty"`
	DurationMs  int64       `json:"durationMs,omitempty"`
	Regressions []string    `json:"regressions,omitempty"`
	Preexisting []string    `json:"preexisting,omitempty"`
	Tail        string      `json:"tail,omitempty"`
}

// Options selects the tree to verify. Repo is the main clone (where
// .dispatch/verify.json usually lives); it defaults to Worktree.
type Options struct {
	Repo     string
	Worktree string
	BaseSHA  string
}

// Example is the verify.json written by `dispatch verify --init`.
const Example = `{
  "checks": [
    {
      "name": "test",
      "argv": ["go", "test", "./..."],
      "timeout": "10m",
      "required": true,
      "parse": "gotest"
    }
  ]
}
`

// ConfigPath is <repo>/.dispatch/verify.json.
func ConfigPath(repo string) string {
	return filepath.Join(repo, ".dispatch", "verify.json")
}

// Run executes project checks and the cheap diff checks. A missing config
// with no go.mod is unverified, which is not a pass.
func Run(ctx context.Context, opt Options) (res Result, err error) {
	start := time.Now()
	finished := false
	defer func() {
		res.DurationMs = time.Since(start).Milliseconds()
		if finished {
			return
		}
		if err == nil {
			err = ctx.Err()
		}
		if res.Status == "" || res.Status == StatusVerified || res.Status == StatusUnverified {
			res.Status = StatusError
		}
	}()

	if opt.Worktree == "" {
		res.Status = StatusError
		finished = true
		return res, fmt.Errorf("verify: empty worktree")
	}
	if opt.Repo == "" {
		opt.Repo = opt.Worktree
	}
	cfg, err := loadConfig(opt.Repo, opt.Worktree)
	if err != nil {
		res.Status = StatusError
		finished = true
		return res, err
	}

	var checks []CheckResult
	if opt.BaseSHA != "" {
		warnings, cheap, cerr := cheapChecks(ctx, opt.Worktree, opt.BaseSHA)
		if cerr != nil {
			if ctx.Err() != nil {
				return res, cerr
			}
			res.Status = StatusError
			finished = true
			return res, cerr
		}
		res.Warnings = warnings
		checks = append(checks, cheap...)
	} else if !cfg.skip {
		res.Warnings = append(res.Warnings, "cheap checks skipped: no base SHA")
	}

	var ran []executed
	for _, c := range cfg.checks {
		if err := ctx.Err(); err != nil {
			res.Checks = checks
			return res, err
		}
		ex := runCheck(ctx, c, opt.Worktree)
		ran = append(ran, ex)
		if ctx.Err() != nil {
			res.Checks = append(checks, ex.bare())
			return res, ctx.Err()
		}
	}

	var base map[string]cachedCheck
	if needs := failedChecks(cfg.checks, ran); len(needs) > 0 {
		if opt.BaseSHA == "" {
			res.Checks = append(checks, bareAll(ran)...)
			res.Status = StatusError
			finished = true
			return res, fmt.Errorf("no base SHA to compare failures")
		}
		var cached bool
		var berr error
		base, cached, berr = baseline(ctx, opt.Repo, opt.BaseSHA, cfg.checks, needs)
		res.BaselineCached = cached
		if berr != nil {
			res.Checks = append(checks, bareAll(ran)...)
			if ctx.Err() != nil {
				return res, berr
			}
			res.Status = StatusError
			finished = true
			return res, berr
		}
	}

	reproved, verifierErr, executedAny := false, false, false
	for _, ex := range ran {
		cr := ex.judge(base)
		if ex.status != CheckSkipped {
			executedAny = true
		}
		if ex.check.Required && cr.Status == CheckError {
			verifierErr = true
		}
		if ex.check.Required && len(cr.Regressions) > 0 {
			reproved = true
		}
		checks = append(checks, cr)
	}
	for _, c := range checks {
		if len(c.Regressions) > 0 && (c.Name == checkDiff || c.Name == checkConflicts) {
			reproved = true
		}
	}
	res.Checks = checks
	switch {
	case reproved:
		res.Status = StatusFailed
	case verifierErr:
		res.Status = StatusError
	case cfg.skip || !executedAny:
		res.Status = StatusUnverified
	default:
		res.Status = StatusVerified
	}
	finished = true
	return res, nil
}

type fileConfig struct {
	Checks []checkConfig `json:"checks"`
}

type checkConfig struct {
	Name     string   `json:"name"`
	Argv     []string `json:"argv"`
	Timeout  string   `json:"timeout,omitempty"`
	Required *bool    `json:"required,omitempty"`
	Parse    string   `json:"parse,omitempty"`
}

type check struct {
	Name     string
	Argv     []string
	Timeout  time.Duration
	Required bool
	Parse    string
}

type loaded struct {
	checks []check
	skip   bool
}

// HasConfig reports whether repo has a verify config (.dispatch/verify.json)
// or autodiscoverable checks (e.g. go.mod).
func HasConfig(repo string) bool {
	cfg, err := loadConfig(repo, repo)
	return err == nil && !cfg.skip
}

func loadConfig(repo, worktree string) (loaded, error) {
	b, path, err := readConfig(repo, worktree)
	if err != nil {
		return loaded{}, err
	}
	if b == nil {
		if _, err := os.Stat(filepath.Join(worktree, "go.mod")); err == nil {
			return loaded{checks: discovered()}, nil
		}
		return loaded{skip: true}, nil
	}
	var f fileConfig
	if err := json.Unmarshal(b, &f); err != nil {
		return loaded{}, fmt.Errorf("%s: %w", path, err)
	}
	if len(f.Checks) == 0 {
		return loaded{skip: true}, nil
	}
	checks, err := normalize(path, f.Checks)
	if err != nil {
		return loaded{}, err
	}
	return loaded{checks: checks}, nil
}

// readConfig prefers the main checkout. A gitignored .dispatch/verify.json
// is not present in a worktree cut from HEAD, and the agent must not be
// able to drop checks by editing only the worktree copy.
func readConfig(repo, worktree string) ([]byte, string, error) {
	candidates := []string{ConfigPath(repo)}
	if worktree != repo {
		candidates = append(candidates, ConfigPath(worktree))
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, p, err
		}
		return b, p, nil
	}
	return nil, "", nil
}

func discovered() []check {
	return []check{
		{Name: "vet", Argv: []string{"go", "vet", "./..."}, Timeout: defaultTimeout, Required: true, Parse: "none"},
		{Name: "test", Argv: []string{"go", "test", "./..."}, Timeout: defaultTimeout, Required: true, Parse: "gotest"},
	}
}

func normalize(path string, in []checkConfig) ([]check, error) {
	seen := map[string]bool{}
	out := make([]check, 0, len(in))
	for i, c := range in {
		name := c.Name
		if name == "" {
			name = fmt.Sprintf("check-%d", i+1)
		}
		if seen[name] {
			return nil, fmt.Errorf("%s: duplicate check %q", path, name)
		}
		seen[name] = true
		if len(c.Argv) == 0 || c.Argv[0] == "" {
			return nil, fmt.Errorf("%s: check %q has empty argv", path, name)
		}
		mode := c.Parse
		if mode == "" {
			mode = "none"
		}
		if mode != "none" && mode != "gotest" {
			return nil, fmt.Errorf("%s: check %q: parse %q (want gotest|none)", path, name, mode)
		}
		timeout := defaultTimeout
		if c.Timeout != "" {
			d, err := time.ParseDuration(c.Timeout)
			if err != nil {
				return nil, fmt.Errorf("%s: check %q: timeout: %w", path, name, err)
			}
			if d <= 0 {
				return nil, fmt.Errorf("%s: check %q: timeout must be positive", path, name)
			}
			timeout = d
		}
		required := true
		if c.Required != nil {
			required = *c.Required
		}
		out = append(out, check{
			Name: name, Argv: append([]string{}, c.Argv...), Timeout: timeout,
			Required: required, Parse: mode,
		})
	}
	return out, nil
}

func configHash(checks []check) string {
	type wire struct {
		Name     string   `json:"name"`
		Argv     []string `json:"argv"`
		Timeout  string   `json:"timeout"`
		Required bool     `json:"required"`
		Parse    string   `json:"parse"`
	}
	w := make([]wire, len(checks))
	for i, c := range checks {
		w[i] = wire{c.Name, c.Argv, c.Timeout.String(), c.Required, c.Parse}
	}
	b, _ := json.Marshal(w)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func dispatchHome() string {
	if h := os.Getenv("DISPATCH_HOME"); h != "" {
		return h
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".dispatch")
	}
	return filepath.Join(u, ".dispatch")
}
