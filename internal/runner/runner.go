// Package runner executes a tiered request: pick candidates, rank by pace,
// fall back on failure, and persist the lifecycle.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"dispatch/internal/adapter"
	"dispatch/internal/pace"
	"dispatch/internal/router"
	"dispatch/internal/sandbox"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
	"dispatch/internal/worktree"
)

const (
	depthEnv       = "DISPATCH_DEPTH"
	noDelegateEnv  = "DISPATCH_NO_DELEGATE"
	runIDEnv       = "DISPATCH_RUN_ID"
	parentRunIDEnv = "DISPATCH_PARENT_RUN_ID"
)

var ErrRecursive = errors.New("refusing recursive delegation (DISPATCH_DEPTH set)")

// rateLimited matches provider errors that mean "stop sending work here".
var rateLimited = regexp.MustCompile(`(?i)rate.?limit|quota|usage limit|too many requests|\b429\b|resource_exhausted|exhausted`)

const cooldownFor = time.Hour

type Deps struct {
	Adapters []adapter.Adapter
	Table    tiers.Table
	Store    store.Store
	Now      func() time.Time
	// Installed reports whether a binary is available; defaults to exec.LookPath.
	Installed func(binary string) bool
	// WorktreeDir overrides where write-run worktrees are created. When
	// empty they go in <repo>/.dispatch/worktrees, which keeps them under
	// the repo path that agent permission rules usually already cover.
	WorktreeDir string
}

// Options describes a request. For resumes, Resume names the earlier run.
type Options struct {
	Tier       adapter.Tier
	Prompt     string
	Dir        string
	ReadOnly   bool
	Timeout    time.Duration
	NoWorktree bool
	Resume     string
	// Subagent profile: name for the record, soft agent preferences.
	Profile       string
	Prefer, Avoid []string
}

func (d Deps) installed(bin string) bool {
	if d.Installed != nil {
		return d.Installed(bin)
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

func (d Deps) adapter(name string) adapter.Adapter {
	for _, a := range d.Adapters {
		if a.Name() == name {
			return a
		}
	}
	return nil
}

// Candidates resolves the tier table against installed adapters, attaches
// quota, pace, history and cooldowns, and ranks them. Table order is kept
// among equals.
func (d Deps) Candidates(ctx context.Context, tier adapter.Tier, needReadOnly bool, prefer, avoid []string) []router.Candidate {
	var cs []router.Candidate
	for _, e := range d.Table.Tiers[tier] {
		a := d.adapter(e.Agent)
		if a == nil || !d.installed(a.Binary()) {
			continue
		}
		if needReadOnly && !a.Capabilities().ReadOnly {
			continue
		}
		c := router.Candidate{Adapter: a, Model: e.Model}
		if slices.Contains(prefer, e.Agent) {
			c.Bias += router.PreferBias
		}
		if slices.Contains(avoid, e.Agent) {
			c.Bias += router.AvoidBias
		}
		cs = append(cs, c)
	}

	// One quota fetch per agent, in parallel; ModelQuoter adapters resolve
	// per model, others share one quota across their models.
	byAgent := map[string][]int{}
	for i, c := range cs {
		byAgent[c.Adapter.Name()] = append(byAgent[c.Adapter.Name()], i)
	}
	var wg sync.WaitGroup
	for _, idxs := range byAgent {
		wg.Add(1)
		go func(idxs []int) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, 100*time.Second)
			defer cancel()
			a := cs[idxs[0]].Adapter
			if mq, ok := a.(adapter.ModelQuoter); ok {
				models := make([]string, len(idxs))
				for k, i := range idxs {
					models[k] = cs[i].Model
				}
				if qs, err := mq.ModelQuotas(qctx, models); err == nil {
					for _, i := range idxs {
						if q, ok := qs[cs[i].Model]; ok {
							q := q
							cs[i].Quota = &q
						}
					}
				}
				return
			}
			if q, err := a.Quota(qctx); err == nil {
				if q.Key == "" {
					q.Key = a.Name()
				}
				for _, i := range idxs {
					q := q
					cs[i].Quota = &q
				}
			}
		}(idxs)
	}
	wg.Wait()

	now := d.Now()
	cool := d.Store.Cooldowns(now)
	recorded := map[string]bool{}
	for i := range cs {
		c := &cs[i]
		_, c.CooledDown = cool[c.Adapter.Name()]
		if c.Quota == nil {
			continue
		}
		router.Evaluate(c, now)
		key := c.Quota.Key
		if !recorded[key] {
			recorded[key] = true
			d.Store.RecordSnapshot(key, pace.Snapshot{
				UsedPercent: c.Quota.UsedPercent, ResetsAt: c.Quota.ResetsAt,
				WindowMinutes: c.Quota.WindowMinutes, SampledAt: now,
			})
		}
		if c.Quota.WindowMinutes == 7*24*60 {
			h := pace.Historical(d.Store.Snapshots(key), c.Quota.UsedPercent, c.Quota.ResetsAt, now)
			if h.Valid {
				c.Hist = &h
			}
		}
	}
	return router.Rank(cs)
}

// Begin validates the request and persists a PENDING record.
func (d Deps) Begin(o Options) (store.Record, error) {
	if os.Getenv(depthEnv) != "" || os.Getenv(noDelegateEnv) != "" {
		return store.Record{}, ErrRecursive
	}
	rec := store.Record{
		ID: store.NewID(), Status: store.Pending, Tier: string(o.Tier),
		Prompt: o.Prompt, Dir: o.Dir, ReadOnly: o.ReadOnly, CreatedAt: d.Now(),
		TimeoutSec: int(o.Timeout / time.Second), NoWorktree: o.NoWorktree,
		Profile: o.Profile, Prefer: o.Prefer, Avoid: o.Avoid,
	}
	if o.Resume != "" {
		prev, err := d.Store.Get(o.Resume)
		if err != nil {
			return rec, fmt.Errorf("resume %s: %w", o.Resume, err)
		}
		if prev.SessionID == "" || prev.Agent == "" {
			return rec, fmt.Errorf("run %s has no resumable session", prev.ID)
		}
		rec.ResumedOf, rec.Tier = prev.ID, prev.Tier
		rec.Agent, rec.Model, rec.SessionID = prev.Agent, prev.Model, prev.SessionID
		rec.Repo, rec.Worktree, rec.Branch = prev.Repo, prev.Worktree, prev.Branch
		if rec.Dir == "" {
			rec.Dir = prev.Dir
		}
	} else if !adapter.Tier(rec.Tier).Valid() {
		return rec, fmt.Errorf("invalid tier %q (want easy|normal|hard)", rec.Tier)
	}
	return rec, d.Store.Append(rec)
}

// Run is Begin + Execute.
func (d Deps) Run(ctx context.Context, o Options) (store.Record, error) {
	rec, err := d.Begin(o)
	if err != nil {
		return rec, err
	}
	return d.Execute(ctx, rec)
}

// Execute runs a record created by Begin and returns its final state.
func (d Deps) Execute(ctx context.Context, rec store.Record) (store.Record, error) {
	if os.Getenv(depthEnv) != "" || os.Getenv(noDelegateEnv) != "" {
		return rec, ErrRecursive
	}
	rec.PID = os.Getpid()
	timeout := time.Duration(rec.TimeoutSec) * time.Second

	finish := func(s store.Status, msg string) (store.Record, error) {
		rec.Status, rec.Error, rec.FinishedAt = s, msg, d.Now()
		rec.Output = sandbox.Redact(rec.Output)
		rec.Error = sandbox.Redact(rec.Error)
		rec.Note = sandbox.Redact(rec.Note)
		for i := range rec.Attempts {
			rec.Attempts[i].Error = sandbox.Redact(rec.Attempts[i].Error)
		}
		return rec, d.Store.Append(rec)
	}

	extra := []string{
		depthEnv + "=1",
		runIDEnv + "=" + rec.ID,
		noDelegateEnv + "=1",
	}
	if parent := os.Getenv(runIDEnv); parent != "" {
		extra = append(extra, parentRunIDEnv+"="+parent)
	}
	childEnv := sandbox.Environ(extra...)

	dir := rec.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	dir, _ = filepath.Abs(dir)

	var cands []router.Candidate
	resumeSession := ""
	if rec.ResumedOf != "" {
		a := d.adapter(rec.Agent)
		if a == nil || !d.installed(a.Binary()) {
			return finish(store.Failed, "agent "+rec.Agent+" is not available to resume")
		}
		cands = []router.Candidate{{Adapter: a, Model: rec.Model}}
		resumeSession = rec.SessionID
		if rec.Worktree != "" {
			if _, err := os.Stat(rec.Worktree); err == nil {
				if root := worktree.Root(rec.Worktree); root != "" {
					dir = rec.Worktree
				}
			} else {
				rec.Note = "previous worktree is gone; resumed in " + dir
			}
		}
	} else {
		cands = d.Candidates(ctx, adapter.Tier(rec.Tier), rec.ReadOnly, rec.Prefer, rec.Avoid)
		if len(cands) == 0 {
			return finish(store.Failed, "no installed agent serves tier "+rec.Tier)
		}
		if !rec.ReadOnly && !rec.NoWorktree {
			if root := worktree.Root(dir); root != "" && worktree.HasCommits(root) {
				base := d.WorktreeDir
				if base == "" {
					base = filepath.Join(root, ".dispatch", "worktrees")
					worktree.Exclude(root, ".dispatch/")
				}
				path := filepath.Join(base, rec.ID)
				branch, err := worktree.Create(root, path, rec.ID)
				if err != nil {
					return finish(store.Failed, "worktree: "+err.Error())
				}
				rec.Repo, rec.Worktree, rec.Branch = root, path, branch
				dir = filepath.Join(path, worktree.Rel(root, dir))
				if worktree.Dirty(root) {
					rec.Note = "checkout has uncommitted changes; the worktree was cut from HEAD and does not include them"
				}
			}
		}
	}

	for _, c := range cands {
		rec.Status, rec.Agent, rec.Model = store.Running, c.Adapter.Name(), c.Model
		if err := d.Store.Append(rec); err != nil {
			return rec, err
		}
		res, err := c.Adapter.Run(ctx, adapter.Request{
			Prompt: rec.Prompt, Tier: adapter.Tier(rec.Tier), Dir: dir, ReadOnly: rec.ReadOnly,
			Timeout: timeout, ResumeSession: resumeSession, Env: childEnv,
		}, c.Model)
		rec.ExitCode, rec.SessionID = res.ExitCode, res.SessionID
		switch {
		case err == nil:
			rec.Output = res.Output
			if rec.Worktree != "" && !rec.ReadOnly {
				if ok, cerr := worktree.Commit(rec.Worktree, rec.ID); cerr != nil {
					rec.Note = joinNote(rec.Note, "commit failed: "+cerr.Error())
				} else if ok {
					rec.Note = joinNote(rec.Note, "changes committed on "+rec.Branch)
				} else {
					rec.Note = joinNote(rec.Note, "no file changes")
				}
			}
			return finish(store.Completed, "")
		case ctx.Err() != nil:
			return finish(store.Cancelled, ctx.Err().Error())
		case errors.Is(err, context.DeadlineExceeded):
			return finish(store.TimedOut, "timeout after "+timeout.String())
		}
		if rateLimited.MatchString(err.Error()) {
			d.Store.SetCooldown(c.Adapter.Name(), d.Now().Add(cooldownFor), d.Now())
		}
		rec.Attempts = append(rec.Attempts, store.Attempt{Agent: c.Adapter.Name(), Model: c.Model, Error: err.Error()})
	}
	last := rec.Attempts[len(rec.Attempts)-1]
	return finish(store.Failed, fmt.Sprintf("all %d candidates failed; last: %s/%s: %s", len(rec.Attempts), last.Agent, last.Model, last.Error))
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func terminal(s store.Status) bool {
	return s == store.Completed || s == store.Failed || s == store.Cancelled || s == store.TimedOut
}

// Wait polls until the run reaches a terminal status or ctx ends.
func (d Deps) Wait(ctx context.Context, id string, poll time.Duration) (store.Record, error) {
	for {
		rec, err := d.Store.Get(id)
		if err != nil {
			return rec, err
		}
		if terminal(rec.Status) {
			return rec, nil
		}
		if rec.PID > 0 && !alive(rec.PID) {
			// Process died without a final record: mark it so waiters finish.
			rec.Status, rec.FinishedAt = store.Failed, d.Now()
			rec.Error = joinNote(rec.Error, "runner process exited unexpectedly")
			d.Store.Append(rec)
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return rec, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Cancel asks the process running id to stop; its handler records
// CANCELLED. A run whose process is already gone is marked directly.
func (d Deps) Cancel(ctx context.Context, id string) (store.Record, error) {
	rec, err := d.Store.Get(id)
	if err != nil {
		return rec, err
	}
	if terminal(rec.Status) {
		return rec, fmt.Errorf("run %s already %s", rec.ID, rec.Status)
	}
	if rec.PID > 0 && alive(rec.PID) {
		if err := syscall.Kill(rec.PID, syscall.SIGTERM); err != nil {
			return rec, err
		}
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if out, err := d.Wait(wctx, rec.ID, 200*time.Millisecond); err == nil {
			return out, nil
		}
	}
	rec, _ = d.Store.Get(id)
	if !terminal(rec.Status) {
		rec.Status, rec.FinishedAt = store.Cancelled, d.Now()
		rec.Error = "cancelled"
		d.Store.Append(rec)
	}
	return rec, nil
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Summary returns a compact head of output: at most maxLines lines and
// maxBytes bytes (cut on a rune boundary), and whether it was truncated.
func Summary(output string, maxLines, maxBytes int) (string, bool) {
	end, lines := 0, 0
	for end < len(output) && lines < maxLines && end < maxBytes {
		if output[end] == '\n' {
			lines++
		}
		end++
	}
	truncated := end < len(output)
	for end > 0 && end < len(output) && !utf8.RuneStart(output[end]) {
		end--
	}
	return output[:end], truncated
}

// Page returns lines [offset, offset+limit) of text, the total line count
// and whether more lines follow.
func Page(text string, offset, limit int) (content string, total int, more bool) {
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			lines = append(lines, text[start:i])
			start = i + 1
		}
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	total = len(lines)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < total {
		end = offset + limit
	}
	out := ""
	for i := offset; i < end; i++ {
		out += lines[i] + "\n"
	}
	return out, total, end < total
}
