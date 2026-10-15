package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"dispatch/internal/process"
	"dispatch/internal/sandbox"
)

const tailLimit = 16 << 10

type executed struct {
	check    check
	status   CheckStatus
	exit     int
	duration time.Duration
	tail     string
	ids      []string
	err      error
}

func (ex executed) bare() CheckResult {
	cr := CheckResult{
		Name: ex.check.Name, Status: ex.status, ExitCode: ex.exit,
		DurationMs: ex.duration.Milliseconds(), Tail: ex.tail,
	}
	if cr.Status == "" {
		cr.Status = CheckError
	}
	return cr
}

func (ex executed) needsBaseline() bool {
	return ex.status == CheckFailed || ex.status == CheckTimeout
}

func (ex executed) judge(base map[string]cachedCheck) CheckResult {
	cr := ex.bare()
	if ex.status != CheckFailed && ex.status != CheckTimeout {
		return cr
	}
	cached, ok := base[ex.check.Name]
	if len(ex.ids) == 0 {
		label := fmt.Sprintf("%s:exit%d", ex.check.Name, ex.exit)
		if ex.status == CheckTimeout {
			label = ex.check.Name + ":timeout"
		}
		// No comparable identity. Reprove only when the baseline did not
		// fail the same way (it passed, or it failed with real identities).
		if ok && !cached.Passed && len(cached.IDs) == 0 {
			cr.Preexisting = []string{label}
			return cr
		}
		cr.Regressions = []string{label}
		return cr
	}
	have := map[string]bool{}
	if ok {
		for _, id := range cached.IDs {
			have[id] = true
		}
	}
	for _, id := range ex.ids {
		if ok && have[id] {
			cr.Preexisting = append(cr.Preexisting, id)
		} else {
			cr.Regressions = append(cr.Regressions, id)
		}
	}
	return cr
}

func failedChecks(all []check, ran []executed) []check {
	want := map[string]bool{}
	for _, ex := range ran {
		if ex.needsBaseline() {
			want[ex.check.Name] = true
		}
	}
	var out []check
	for _, c := range all {
		if want[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

func bareAll(ran []executed) []CheckResult {
	out := make([]CheckResult, len(ran))
	for i, ex := range ran {
		out[i] = ex.bare()
	}
	return out
}

type capture struct {
	mu    sync.Mutex
	tail  []byte
	line  []byte
	parse *goTestState // nil unless parse=gotest
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tail = append(c.tail, p...)
	if len(c.tail) > tailLimit {
		c.tail = append([]byte(nil), c.tail[len(c.tail)-tailLimit:]...)
	}
	if c.parse == nil {
		return len(p), nil
	}
	c.line = append(c.line, p...)
	for {
		i := bytes.IndexByte(c.line, '\n')
		if i < 0 {
			break
		}
		c.parse.line(c.line[:i])
		c.line = c.line[i+1:]
	}
	if len(c.line) > 1<<20 {
		c.line = nil
	}
	return len(p), nil
}

func (c *capture) finish() (tail string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.parse != nil && len(c.line) > 0 {
		c.parse.line(c.line)
	}
	b := c.tail
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	if c.parse != nil {
		ids = c.parse.ids()
	}
	return sandbox.Redact(string(b)), ids
}

func runCheck(ctx context.Context, c check, dir string) executed {
	ex := executed{check: c}
	argv := append([]string{}, c.Argv...)
	if c.Parse == "gotest" {
		argv = withGoTestJSON(argv)
	}
	if len(argv) == 0 || argv[0] == "" {
		ex.status, ex.err = CheckError, fmt.Errorf("empty argv")
		return ex
	}
	if !strings.Contains(argv[0], "/") {
		if _, err := exec.LookPath(argv[0]); err != nil {
			ex.err = err
			if c.Required {
				ex.status = CheckError
				ex.tail = sandbox.Redact(err.Error())
			} else {
				ex.status = CheckSkipped
			}
			return ex
		}
	}
	tctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = sandbox.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	cap := &capture{}
	if c.Parse == "gotest" {
		cap.parse = &goTestState{}
	}
	cmd.Stdout, cmd.Stderr = cap, cap
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			killTree(cmd.Process.Pid)
		}
		return nil
	}
	started := time.Now()
	err := cmd.Run()
	ex.duration = time.Since(started)
	ex.tail, ex.ids = cap.finish()
	timedOut := errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	if ctx.Err() != nil && !timedOut {
		ex.status, ex.err = CheckError, ctx.Err()
		return ex
	}
	if timedOut {
		ex.status, ex.exit = CheckTimeout, -1
		ex.ids = append(ex.ids, c.Name+":timeout")
		ex.ids = unique(ex.ids)
		return ex
	}
	if err == nil {
		ex.status = CheckPassed
		ex.ids = nil
		return ex
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		ex.exit = ee.ExitCode()
		ex.status = CheckFailed
		if c.Parse == "none" {
			ex.ids = []string{fmt.Sprintf("%s:exit%d", c.Name, ex.exit)}
		} else {
			ex.ids = unique(ex.ids)
		}
		return ex
	}
	ex.status, ex.err = CheckError, err
	if ex.tail == "" {
		ex.tail = sandbox.Redact(err.Error())
	}
	return ex
}

func unique(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func killTree(pid int) {
	if pid <= 0 {
		return
	}
	tree, err := process.Capture(pid)
	if err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
		return
	}
	_ = tree.Signal(syscall.SIGKILL)
}
