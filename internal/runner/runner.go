// Package runner executes a tiered request: pick candidates, rank by pace
// and optional priority weights, fall back on failure, and persist the lifecycle.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dispatch/internal/adapter"
	"dispatch/internal/config"
	"dispatch/internal/pace"
	"dispatch/internal/router"
	"dispatch/internal/sandbox"
	"dispatch/internal/stats"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
	"dispatch/internal/verify"
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
	// Policy is the effective global/project routing policy.
	Policy config.File
}

// Options describes a request. For resumes, Resume names the earlier run.
type Options struct {
	Filters    store.Filters
	Tier       adapter.Tier
	Prompt     string
	Dir        string
	ReadOnly   bool
	Timeout    time.Duration
	NoWorktree bool
	NoVerify   bool
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
func (d Deps) Candidates(ctx context.Context, tier adapter.Tier, needReadOnly bool, prefer, avoid []string) ([]router.Candidate, []string) {
	cs, rejected := d.selectCandidates(Options{Tier: tier, ReadOnly: needReadOnly, Prefer: prefer, Avoid: avoid})
	return d.evaluate(ctx, cs, true), rejected
}

func (d Deps) evaluate(ctx context.Context, cs []router.Candidate, live bool) []router.Candidate {
	return router.Rank(d.observe(ctx, cs, live))
}

func (d Deps) observe(ctx context.Context, cs []router.Candidate, live bool) []router.Candidate {
	// One quota fetch per agent, in parallel; ModelQuoter adapters resolve
	// per model, others share one quota across their models.
	byAgent := map[string][]int{}
	for i, c := range cs {
		byAgent[c.Adapter.Name()] = append(byAgent[c.Adapter.Name()], i)
	}
	var wg sync.WaitGroup
	for _, idxs := range byAgent {
		if !live {
			continue
		}
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

	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	cool := d.Store.Cooldowns(now)
	recorded := map[string]bool{}
	for i := range cs {
		c := &cs[i]
		_, c.CooledDown = cool[c.Adapter.Name()]
		if !live {
			key := c.Adapter.Name()
			if key == "antigravity" {
				key += ":Claude and GPT models"
				if strings.Contains(strings.ToLower(c.Model), "gemini") {
					key = "antigravity:Gemini Models"
				}
			}
			for _, snap := range d.Store.Snapshots(key) {
				if snap.ResetsAt.After(now) {
					c.Quota = &adapter.Quota{Key: key, UsedPercent: snap.UsedPercent, ResetsAt: snap.ResetsAt, WindowMinutes: snap.WindowMinutes}
				}
			}
		}
		if c.Quota == nil {
			continue
		}
		router.Evaluate(c, now)
		key := c.Quota.Key
		if live && !recorded[key] {
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
	d.attachEvidence(cs)
	return cs
}

// attachEvidence copies tier-segmented measurements onto candidates.
// A read error leaves the static prior in place.
func (d Deps) attachEvidence(cs []router.Candidate) {
	if d.Store.Path == "" {
		return
	}
	recs, err := d.Store.List()
	if err != nil || len(recs) == 0 {
		return
	}
	ev := stats.EvidenceByTierModel(recs)
	for i := range cs {
		if e, ok := ev[stats.EvidenceKey(cs[i].Tier, cs[i].Model)]; ok {
			cs[i].Evidence = e
		}
	}
}

// block probes the CLI when it can. DEGRADED still runs and returns a note
// via the record. A missing flag for a capability this run requires is refused.
func (d Deps) block(ctx context.Context, rec *store.Record, c router.Candidate, resume bool) string {
	p, ok := c.Adapter.(adapter.Prober)
	if !ok {
		return ""
	}
	report := p.Probe(ctx)
	if report.Status != adapter.StatusDegraded {
		return ""
	}
	if rec.ReadOnly {
		if miss := missingFlags(report, "readOnly"); len(miss) > 0 {
			return fmt.Sprintf("%s does not support readOnly (missing %s)", c.Adapter.Name(), strings.Join(miss, ", "))
		}
	} else if miss := missingFlags(report, "fileEditing"); len(miss) > 0 {
		return fmt.Sprintf("%s does not support fileEditing (missing %s)", c.Adapter.Name(), strings.Join(miss, ", "))
	}
	if c.Model != "" {
		for _, f := range report.Flags {
			if !f.Present && (f.Name == "--model" || f.Name == "-m") {
				return "model selection unavailable (missing " + f.Name + ")"
			}
		}
	}
	if c.Effort != "" && c.Adapter.Capabilities().EffortMode == "config" {
		for _, f := range report.Flags {
			if !f.Present && f.Name == "-c" {
				return "effort configuration unavailable (missing -c)"
			}
		}
	}
	if resume {
		if miss := missingFlags(report, "sessionResume"); len(miss) > 0 {
			return fmt.Sprintf("%s does not support session resume (missing %s)", c.Adapter.Name(), strings.Join(miss, ", "))
		}
	}
	detail := strings.Join(report.Notes, "; ")
	if detail == "" {
		detail = "capability flags missing"
	}
	rec.Note = joinNote(rec.Note, c.Adapter.Name()+" probe DEGRADED: "+detail)
	return ""
}

func missingFlags(r adapter.ProbeReport, capability string) []string {
	var out []string
	for _, f := range r.Flags {
		if !f.Present && f.Capability == capability {
			out = append(out, f.Name)
		}
	}
	return out
}

// Begin validates the request and persists a PENDING record.
func (d Deps) Begin(o Options) (store.Record, error) {
	if os.Getenv(depthEnv) != "" || os.Getenv(noDelegateEnv) != "" {
		return store.Record{}, ErrRecursive
	}
	if err := normalizePriority(&o); err != nil {
		return store.Record{}, err
	}
	rec := store.Record{
		PID: os.Getpid(), ProcStart: processStart(context.Background(), os.Getpid()),
		ID: store.NewID(), Status: store.Pending, Tier: string(o.Tier),
		Prompt: o.Prompt, Dir: o.Dir, ReadOnly: o.ReadOnly, CreatedAt: d.Now(),
		TimeoutSec: int(o.Timeout / time.Second), NoWorktree: o.NoWorktree, NoVerify: o.NoVerify,
		Profile: o.Profile, Prefer: o.Prefer, Avoid: o.Avoid,
		Requested: &o.Filters,
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
		rec.BaseSHA = prev.BaseSHA
		rec.Provider, rec.Effort, rec.Resolved = prev.Provider, prev.Effort, prev.Resolved
		if o.Filters.Agent != "" && o.Filters.Agent != prev.Agent || o.Filters.Model != "" && !matchesModel(prev.Model, o.Filters.Model) || o.Filters.Provider != "" && o.Filters.Provider != ProviderOf(prev.Agent, prev.Model) || o.Filters.Effort != "" && o.Filters.Effort != prev.Effort {
			return rec, fmt.Errorf("resume filters conflict with existing session")
		}
		if rec.Dir == "" {
			rec.Dir = prev.Dir
		}
	} else if !adapter.Tier(rec.Tier).Valid() && !(rec.Tier == "" && (o.Filters.Model != "" || o.Filters.Provider != "")) {
		return rec, fmt.Errorf("invalid tier %q (want easy|normal|hard)", rec.Tier)
	}
	if o.Resume != "" {
		o.Tier = adapter.Tier(rec.Tier)
	}
	if err := validateOptions(o); err != nil {
		return rec, err
	}
	return rec, d.Store.Append(rec)
}

// Run is Begin + Execute.
func (d Deps) Run(ctx context.Context, o Options) (store.Record, error) {
	if err := d.Reconcile(ctx); err != nil {
		return store.Record{}, err
	}
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
	if terminal(rec.Status) {
		return rec, fmt.Errorf("run %s already %s", rec.ID, rec.Status)
	}
	rec.PID, rec.ProcStart, rec.StartedAt = os.Getpid(), processStart(ctx, os.Getpid()), d.Now()
	if !rec.CreatedAt.IsZero() {
		rec.QueuedMs = store.I64(nonNegMs(rec.StartedAt.Sub(rec.CreatedAt)))
	}
	if err := d.Store.Append(rec); err != nil {
		return rec, err
	}
	if err := d.Reconcile(ctx); err != nil {
		return rec, err
	}
	timeout := time.Duration(rec.TimeoutSec) * time.Second
	var agentStart time.Time

	finish := func(s store.Status, msg string) (store.Record, error) {
		rec.Status, rec.Error, rec.FinishedAt = s, msg, d.Now()
		if rec.RunMs == nil && !agentStart.IsZero() {
			rec.RunMs = store.I64(nonNegMs(d.Now().Sub(agentStart)))
		}
		completeTelemetry(&rec, rec.FinishedAt)
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
		if !a.Capabilities().SessionResume {
			return finish(store.Failed, a.Name()+" does not support session resume")
		}
		cands = []router.Candidate{{Adapter: a, Model: rec.Model, Provider: rec.Provider, Effort: rec.Effort}}
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
		var rejected []string
		o := Options{Tier: adapter.Tier(rec.Tier), ReadOnly: rec.ReadOnly, Prefer: rec.Prefer, Avoid: rec.Avoid}
		if rec.Requested != nil {
			o.Filters = *rec.Requested
		}
		if o.Filters.Priority == "" {
			o.Filters.Priority = router.PriorityBalanced
		}
		cands, rejected = d.selectCandidatesWith(ctx, o)
		cands = d.evaluate(ctx, cands, true)
		if o.Filters.Relax && policyRequestReason(d.Policy, o.Filters) == "" {
			fallback := o
			fallback.Filters = store.Filters{Priority: o.Filters.Priority}
			if fallback.Tier == "" {
				fallback.Tier = adapter.TierNormal
			}
			extra, _ := d.selectCandidatesWith(ctx, fallback)
			for _, c := range d.evaluate(ctx, extra, true) {
				duplicate := false
				for _, prev := range cands {
					if prev.Adapter.Name() == c.Adapter.Name() && prev.Model == c.Model && prev.Effort == c.Effort {
						duplicate = true
					}
				}
				if !duplicate {
					c.Note = joinNote(c.Note, "--relax: filters relaxed to tier routing")
					cands = append(cands, c)
				}
			}
		}
		if len(cands) == 0 {
			if len(rejected) > 0 {
				return finish(store.Failed, strings.Join(rejected, "; "))
			}
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
				rec.BaseSHA, err = worktree.Head(path)
				if err != nil {
					return finish(store.Failed, err.Error())
				}
				dir = filepath.Join(path, worktree.Rel(root, dir))
				if worktree.Dirty(root) {
					rec.Note = joinNote(rec.Note, "checkout has uncommitted changes; the worktree was cut from HEAD and does not include them")
				}
			}
		}
	}

	var skipped []string
	for _, c := range cands {
		if _, cooling := d.Store.Cooldowns(d.now())[c.Adapter.Name()]; cooling {
			skipped = append(skipped, c.Adapter.Name()+" in cooldown")
			continue
		}
		if msg := d.block(ctx, &rec, c, resumeSession != ""); msg != "" {
			skipped = append(skipped, msg)
			continue
		}
		if agentStart.IsZero() {
			agentStart = d.Now()
		}
		attemptStart := d.Now()
		rec.AgentPID = 0
		rec.Status, rec.Agent, rec.Model = store.Running, c.Adapter.Name(), c.Model
		rec.Provider = c.Provider
		if rec.Provider == "" {
			rec.Provider = ProviderOf(rec.Agent, rec.Model)
		}
		rec.Effort = c.Effort
		if rec.Effort == "" {
			rec.Effort = EffortOf(rec.Model)
		}
		prio := c.Priority
		if prio == "" && rec.Requested != nil {
			prio = rec.Requested.Priority
		}
		if prio == "" {
			prio = router.PriorityBalanced
		}
		rec.Resolved = &store.Filters{Provider: rec.Provider, Model: rec.Model, Agent: rec.Agent, Effort: rec.Effort, Priority: prio}
		rec.Note = joinNote(rec.Note, c.Note)
		if err := d.Store.Append(rec); err != nil {
			return rec, err
		}
		req := adapter.Request{
			Effort: rec.Effort, Prompt: rec.Prompt, Tier: adapter.Tier(rec.Tier), Dir: dir, ReadOnly: rec.ReadOnly,
			Started: func(pid int) error {
				rec.AgentPID = pid
				return d.Store.Append(rec)
			},
			Timeout: timeout, ResumeSession: resumeSession, Env: childEnv,
		}
		var res adapter.Result
		var err error
		if resumeSession != "" {
			if r, ok := c.Adapter.(adapter.Resumer); ok {
				res, err = r.Resume(ctx, req, c.Model)
			} else {
				res, err = c.Adapter.Run(ctx, req, c.Model)
			}
		} else {
			res, err = c.Adapter.Run(ctx, req, c.Model)
		}
		rec.ExitCode, rec.SessionID = res.ExitCode, res.SessionID
		switch {
		case errors.Is(err, adapter.ErrProcessesAlive):
			return finish(store.Failed, err.Error())
		case err == nil:
			rec.Output = res.Output
			rec.RunMs = store.I64(nonNegMs(d.Now().Sub(agentStart)))
			if rec.Worktree != "" && !rec.ReadOnly {
				committed := true
				if ok, cerr := worktree.Commit(rec.Worktree, rec.ID); cerr != nil {
					committed = false
					rec.Note = joinNote(rec.Note, "commit failed: "+cerr.Error())
				} else if ok {
					rec.Note = joinNote(rec.Note, "changes committed on "+rec.Branch)
				} else {
					rec.Note = joinNote(rec.Note, "no file changes")
				}
				if !rec.NoVerify {
					if !committed {
						rec.Verification = string(verify.StatusError)
						rec.Verify = &verify.Result{Status: verify.StatusError}
						rec.Note = joinNote(rec.Note, "verification skipped: commit failed")
					} else {
						d.applyVerify(ctx, &rec)
					}
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
		rec.Attempts = append(rec.Attempts, store.Attempt{
			Agent: c.Adapter.Name(), Model: c.Model, Error: err.Error(),
			DurationMs: store.I64(nonNegMs(d.Now().Sub(attemptStart))),
			ExitClass:  classOf(err.Error()),
		})
	}
	if len(rec.Attempts) == 0 {
		msg := strings.Join(skipped, "; ")
		if msg == "" {
			msg = "no candidate could run"
		}
		return finish(store.Failed, msg)
	}
	last := rec.Attempts[len(rec.Attempts)-1]
	return finish(store.Failed, fmt.Sprintf("all %d candidates failed; last: %s/%s: %s", len(rec.Attempts), last.Agent, last.Model, last.Error))
}

// applyVerify runs after a successful write commit, including on --resume.
func (d Deps) applyVerify(ctx context.Context, rec *store.Record) {
	if rec.NoVerify || rec.Worktree == "" {
		return
	}
	if st, err := os.Stat(rec.Worktree); err != nil || !st.IsDir() {
		return
	}
	res, err := verify.Run(ctx, verify.Options{
		Repo: rec.Repo, Worktree: rec.Worktree, BaseSHA: rec.BaseSHA,
	})
	rec.Verify = &res
	rec.Verification = string(res.Status)
	rec.VerifyMs = res.DurationMs
	if err != nil {
		rec.Note = joinNote(rec.Note, "verification: "+err.Error())
	}
}

// ExitClass maps a terminal status and the verification verdict.
func ExitClass(status store.Status, verification string) string {
	switch status {
	case store.Completed:
		if verification == string(verify.StatusFailed) {
			return "verify_fail"
		}
		return "ok"
	case store.TimedOut:
		return "timeout"
	case store.Cancelled:
		return "cancelled"
	case store.Failed:
		return "failed"
	default:
		return ""
	}
}

func nonNegMs(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

// completeTelemetry fills provider, effort, diff stats and exit class.
// Nil numeric fields stay nil when the measurement is unknown.
func completeTelemetry(rec *store.Record, now time.Time) {
	if rec.RunMs == nil && !rec.StartedAt.IsZero() && !now.IsZero() && terminal(rec.Status) {
		rec.RunMs = store.I64(nonNegMs(now.Sub(rec.StartedAt)))
	}
	if rec.Provider == "" {
		rec.Provider = ProviderOf(rec.Agent, rec.Model)
	}
	if rec.Effort == "" {
		rec.Effort = EffortOf(rec.Model)
	}
	if rec.FilesChanged == nil && rec.Worktree != "" && rec.BaseSHA != "" {
		fillDiff(rec)
	}
	if terminal(rec.Status) {
		if cls := Classify(*rec); cls != "" {
			rec.ExitClass = cls
		}
	}
}

func fillDiff(rec *store.Record) {
	files, ins, del, linesKnown, err := worktree.NumStat(rec.Worktree, rec.BaseSHA)
	if err != nil {
		return
	}
	rec.FilesChanged = store.I(files)
	if linesKnown {
		rec.Insertions = store.I(ins)
		rec.Deletions = store.I(del)
	}
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" || strings.Contains(a, b) {
		return a
	}
	return a + "; " + b
}

func terminal(s store.Status) bool {
	return s == store.Orphaned || s == store.Completed || s == store.Failed || s == store.Cancelled || s == store.TimedOut
}

// Wait polls until the run reaches a terminal status or ctx ends.
func (d Deps) Wait(ctx context.Context, id string, poll time.Duration) (store.Record, error) {
	for {
		if err := d.Reconcile(ctx); err != nil {
			rec, _ := d.Store.Get(id)
			return rec, err
		}
		rec, err := d.Store.Get(id)
		if err != nil {
			return rec, err
		}
		if terminal(rec.Status) {
			return rec, nil
		}
		select {
		case <-ctx.Done():
			return rec, ctx.Err()
		case <-time.After(poll):
		}
	}
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
