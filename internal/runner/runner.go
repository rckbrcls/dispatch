// Package runner executes a tiered request: pick candidates, rank by pace,
// fall back on failure, and persist the lifecycle.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
	"unicode/utf8"

	"dispatch/internal/adapter"
	"dispatch/internal/router"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
)

const depthEnv = "DISPATCH_DEPTH"

var ErrRecursive = errors.New("refusing recursive delegation (DISPATCH_DEPTH set)")

type Deps struct {
	Adapters []adapter.Adapter
	Table    tiers.Table
	Store    store.Store
	Now      func() time.Time
	// Installed reports whether a binary is available; defaults to exec.LookPath.
	Installed func(binary string) bool
}

type Options struct {
	Tier     adapter.Tier
	Prompt   string
	Dir      string
	ReadOnly bool
	Timeout  time.Duration
}

func (d Deps) installed(bin string) bool {
	if d.Installed != nil {
		return d.Installed(bin)
	}
	_, err := exec.LookPath(bin)
	return err == nil
}

// Candidates resolves the tier table against installed adapters and ranks
// them by quota pace. Table order is preserved among equals.
func (d Deps) Candidates(ctx context.Context, tier adapter.Tier, needReadOnly bool) []router.Candidate {
	byName := map[string]adapter.Adapter{}
	for _, a := range d.Adapters {
		byName[a.Name()] = a
	}
	var cs []router.Candidate
	for _, e := range d.Table.Tiers[tier] {
		a, ok := byName[e.Agent]
		if !ok || !d.installed(a.Binary()) {
			continue
		}
		if needReadOnly && !a.Capabilities().ReadOnly {
			continue
		}
		cs = append(cs, router.Candidate{Adapter: a, Model: e.Model})
	}
	// Fetch quota once per agent, in parallel.
	quotas := map[string]*adapter.Quota{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[string]bool{}
	for _, c := range cs {
		if seen[c.Adapter.Name()] {
			continue
		}
		seen[c.Adapter.Name()] = true
		wg.Add(1)
		go func(a adapter.Adapter) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			if q, err := a.Quota(qctx); err == nil {
				mu.Lock()
				quotas[a.Name()] = &q
				mu.Unlock()
			}
		}(c.Adapter)
	}
	wg.Wait()
	now := d.Now()
	for i := range cs {
		cs[i].Quota = quotas[cs[i].Adapter.Name()]
		router.Evaluate(&cs[i], now)
	}
	return router.Rank(cs)
}

// Run executes the request synchronously and returns the final record.
func (d Deps) Run(ctx context.Context, o Options) (store.Record, error) {
	if os.Getenv(depthEnv) != "" {
		return store.Record{}, ErrRecursive
	}
	os.Setenv(depthEnv, "1") // inherited by every agent subprocess
	if !o.Tier.Valid() {
		return store.Record{}, fmt.Errorf("invalid tier %q (want easy|normal|hard)", o.Tier)
	}
	rec := store.Record{
		ID: store.NewID(), Status: store.Pending, Tier: string(o.Tier),
		Prompt: o.Prompt, Dir: o.Dir, ReadOnly: o.ReadOnly, CreatedAt: d.Now(),
	}
	if err := d.Store.Append(rec); err != nil {
		return rec, err
	}
	finish := func(s store.Status, msg string) (store.Record, error) {
		rec.Status, rec.Error, rec.FinishedAt = s, msg, d.Now()
		return rec, d.Store.Append(rec)
	}

	cands := d.Candidates(ctx, o.Tier, o.ReadOnly)
	if len(cands) == 0 {
		return finish(store.Failed, "no installed agent serves tier "+string(o.Tier))
	}
	for _, c := range cands {
		rec.Status, rec.Agent, rec.Model = store.Running, c.Adapter.Name(), c.Model
		if err := d.Store.Append(rec); err != nil {
			return rec, err
		}
		res, err := c.Adapter.Run(ctx, adapter.Request{
			Prompt: o.Prompt, Tier: o.Tier, Dir: o.Dir, ReadOnly: o.ReadOnly, Timeout: o.Timeout,
		}, c.Model)
		rec.ExitCode = res.ExitCode
		switch {
		case err == nil:
			rec.Output = res.Output
			return finish(store.Completed, "")
		case ctx.Err() != nil:
			return finish(store.Cancelled, ctx.Err().Error())
		case errors.Is(err, context.DeadlineExceeded):
			return finish(store.TimedOut, "timeout after "+o.Timeout.String())
		}
		rec.Attempts = append(rec.Attempts, store.Attempt{Agent: c.Adapter.Name(), Model: c.Model, Error: err.Error()})
	}
	last := rec.Attempts[len(rec.Attempts)-1]
	return finish(store.Failed, fmt.Sprintf("all %d candidates failed; last: %s/%s: %s", len(rec.Attempts), last.Agent, last.Model, last.Error))
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
	if end > maxBytes {
		end = maxBytes
	}
	for end > 0 && !utf8.RuneStart(safeAt(output, end)) {
		end--
	}
	return output[:end], truncated || end < len(output)
}

func safeAt(s string, i int) byte {
	if i >= len(s) {
		return 'a'
	}
	return s[i]
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
