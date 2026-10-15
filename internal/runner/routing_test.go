package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/pace"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
)

type routingFake struct {
	fake
	caps     adapter.Capabilities
	quota    adapter.Quota
	requests *[]adapter.Request
	calls    *int
}

func (f routingFake) Capabilities() adapter.Capabilities { return f.caps }
func (f routingFake) Quota(context.Context) (adapter.Quota, error) {
	if f.calls != nil {
		*f.calls++
	}
	if f.quota.WindowMinutes == 0 {
		return adapter.Quota{}, adapter.ErrNoQuota
	}
	return f.quota, nil
}
func (f routingFake) Run(ctx context.Context, r adapter.Request, m string) (adapter.Result, error) {
	if f.requests != nil {
		*f.requests = append(*f.requests, r)
	}
	return f.fake.Run(ctx, r, m)
}
func routingDeps(t *testing.T) (Deps, *[]string, *[]adapter.Request) {
	t.Helper()
	ran := []string{}
	requests := []adapter.Request{}
	caps := adapter.Capabilities{ReadOnly: true, FileEditing: true, EffortMode: "config", Efforts: []string{"low", "medium", "high", "xhigh"}}
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	a := routingFake{fake: fake{name: "a", ran: &ran}, caps: caps, quota: adapter.Quota{UsedPercent: 40, WindowMinutes: 10080, ResetsAt: now.Add(84 * time.Hour)}, requests: &requests}
	b := a
	b.fake.name = "b"
	b.quota.UsedPercent = 10
	d := deps(t, &ran, a, b)
	d.Now = func() time.Time { return now }
	d.Table = tiers.Table{Tiers: map[adapter.Tier][]tiers.Entry{
		adapter.TierEasy: {{Agent: "a", Model: "gpt-small"}, {Agent: "b", Model: "gpt-small"}, {Agent: "a", Model: "gemini-fast"}},
		adapter.TierHard: {{Agent: "a", Model: "gpt-large"}, {Agent: "b", Model: "gemini-pro"}},
	}}
	return d, &ran, &requests
}
func TestExplicitModelChoosesAgentByPace(t *testing.T) {
	d, ran, reqs := routingDeps(t)
	rec, err := d.Run(context.Background(), Options{Filters: store.Filters{Model: "gpt-small", Effort: "high"}, Prompt: "p", NoWorktree: true})
	if err != nil || rec.Status != store.Completed || rec.Agent != "b" || rec.Effort != "high" {
		t.Fatalf("%+v %v", rec, err)
	}
	if len(*ran) != 1 || (*reqs)[0].Effort != "high" || rec.Requested.Model != "gpt-small" || rec.Resolved.Provider != "openai" {
		t.Fatalf("%+v %v", rec, *reqs)
	}
	persisted, _ := d.Store.Get(rec.ID)
	if persisted.Resolved == nil || Summarize(persisted).Requested.Model != "gpt-small" {
		t.Fatal("routing not persisted in summary")
	}
}
func TestProviderAndTierIntersection(t *testing.T) {
	d, _, _ := routingDeps(t)
	for _, tt := range []struct {
		tier  adapter.Tier
		model string
	}{{adapter.TierEasy, "gpt-small"}, {adapter.TierHard, "gpt-large"}} {
		cs, rejected := d.selectCandidates(Options{Tier: tt.tier, Filters: store.Filters{Provider: "openai"}})
		if len(cs) == 0 || cs[0].Model != tt.model {
			t.Fatalf("%v %v", cs, rejected)
		}
	}
	cs, _ := d.selectCandidates(Options{Tier: adapter.TierHard, Filters: store.Filters{Model: "gpt-small"}})
	if len(cs) != 0 {
		t.Fatal("escaped tier intersection")
	}
}
func TestFixedModelEffortOnlyEvaluatesServingAgents(t *testing.T) {
	d, _, _ := routingDeps(t)
	cs, _ := d.selectCandidates(Options{Filters: store.Filters{Model: "gpt-small", Effort: "xhigh"}})
	for _, c := range d.evaluate(context.Background(), cs, true) {
		if c.Model != "gpt-small" || c.Effort != "xhigh" {
			t.Fatalf("ranked a different model: %+v", c)
		}
	}
	// Filtering occurs before quota/ranking: an unrelated model is never evaluated.
	calls := 0
	a := d.Adapters[0].(routingFake)
	a.fake.name = "unrelated"
	a.calls = &calls
	d.Adapters = append(d.Adapters, a)
	d.Table.Tiers[adapter.TierHard] = append(d.Table.Tiers[adapter.TierHard], tiers.Entry{Agent: "unrelated", Model: "grok-other"})
	cs, _ = d.selectCandidates(Options{Filters: store.Filters{Model: "gpt-small", Effort: "high"}})
	d.evaluate(context.Background(), cs, true)
	if calls != 0 {
		t.Fatal("unrelated model entered ranking")
	}
}
func TestFiltersFailAndRelax(t *testing.T) {
	for _, relax := range []bool{false, true} {
		t.Run(map[bool]string{false: "strict", true: "relax"}[relax], func(t *testing.T) {
			d, ran, _ := routingDeps(t)
			rec, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Filters: store.Filters{Provider: "missing", Relax: relax}, Prompt: "p", NoWorktree: true})
			if err != nil {
				t.Fatal(err)
			}
			if !relax && (rec.Status != store.Failed || len(*ran) != 0 || !strings.Contains(rec.Error, "provider filter")) {
				t.Fatalf("%+v %v", rec, *ran)
			}
			if relax && (rec.Status != store.Completed || !strings.Contains(rec.Note, "--relax")) {
				t.Fatalf("%+v", rec)
			}
		})
	}
}
func TestFallbackNeverChangesFixedModel(t *testing.T) {
	d, ran, _ := routingDeps(t)
	for i, a := range d.Adapters {
		f := a.(routingFake)
		f.fake.err = errors.New("failed")
		d.Adapters[i] = f
	}
	rec, _ := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Filters: store.Filters{Model: "gpt-small"}, Prompt: "p", NoWorktree: true})
	if rec.Status != store.Failed || len(*ran) != 2 {
		t.Fatalf("%+v %v", rec, *ran)
	}
	for _, run := range *ran {
		if !strings.HasSuffix(run, "/gpt-small") {
			t.Fatal(run)
		}
	}
}
func TestUnsupportedEffortRefused(t *testing.T) {
	d, ran, _ := routingDeps(t)
	a := d.Adapters[0].(routingFake)
	a.caps.EffortMode = ""
	a.caps.Efforts = nil
	d.Adapters = []adapter.Adapter{a}
	rec, _ := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Filters: store.Filters{Agent: "a", Model: "gpt-small", Effort: "high"}, Prompt: "p", NoWorktree: true})
	if rec.Status != store.Failed || len(*ran) != 0 || !strings.Contains(rec.Error, "effort unsupported") {
		t.Fatalf("%+v", rec)
	}
	a.caps.EffortMode = "config"
	a.caps.Efforts = []string{"low"}
	d.Adapters = []adapter.Adapter{a}
	cs, why := d.selectCandidates(Options{Tier: adapter.TierEasy, Filters: store.Filters{Agent: "a", Model: "gpt-small", Effort: "high"}})
	if len(cs) != 0 || !strings.Contains(strings.Join(why, ";"), "unsupported") {
		t.Fatal(cs, why)
	}
}
func TestRouteNoSideEffects(t *testing.T) {
	d, ran, _ := routingDeps(t)
	calls := 0
	for i, a := range d.Adapters {
		f := a.(routingFake)
		f.calls = &calls
		d.Adapters[i] = f
	}
	if _, err := d.Store.RecordSnapshot("b", pace.Snapshot{UsedPercent: 5, WindowMinutes: 10080, ResetsAt: d.Now().Add(84 * time.Hour), SampledAt: d.Now()}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(d.Store.Path)
	before, _ := os.ReadDir(dir)
	r, err := d.Route(context.Background(), Options{Filters: store.Filters{Model: "gpt-small", Effort: "high"}})
	after, _ := os.ReadDir(dir)
	if err != nil || r.Winner == nil || r.Winner.Agent != "b" || calls != 0 || len(*ran) != 0 || len(before) != len(after) {
		t.Fatalf("%+v %v calls=%d", r, err, calls)
	}
	if _, err := os.Stat(d.Store.Path); !os.IsNotExist(err) {
		t.Fatal("created runs")
	}
	if _, err := os.Stat(d.WorktreeDir); err == nil {
		entries, _ := os.ReadDir(d.WorktreeDir)
		if len(entries) > 0 {
			t.Fatal("created worktree")
		}
	}
}
func TestOutsideTableAndSafetyConstraints(t *testing.T) {
	d, _, _ := routingDeps(t)
	cs, _ := d.selectCandidates(Options{Tier: adapter.TierHard, Filters: store.Filters{Model: "gpt-new"}})
	if len(cs) != 2 || cs[0].Note == "" {
		t.Fatal(cs)
	}
	cs, why := d.selectCandidates(Options{Filters: store.Filters{Model: "claude-opus", Agent: "a"}})
	if len(cs) != 0 || !strings.Contains(strings.Join(why, ";"), "Claude") {
		t.Fatal(cs, why)
	}
	d.Store.SetCooldown("b", d.Now().Add(time.Hour), d.Now())
	cs, _ = d.selectCandidates(Options{Filters: store.Filters{Model: "gpt-small"}})
	if len(cs) != 1 || cs[0].Adapter.Name() != "a" {
		t.Fatal(cs)
	}
}

func TestSuffixEffortAndReadonly(t *testing.T) {
	d, _, _ := routingDeps(t)
	a := d.Adapters[0].(routingFake)
	a.caps = adapter.Capabilities{ReadOnly: true, FileEditing: true, EffortMode: "model-suffix"}
	d.Adapters = []adapter.Adapter{a}
	d.Table = tiers.Table{Tiers: map[adapter.Tier][]tiers.Entry{adapter.TierEasy: {{Agent: "a", Model: "gemini-fast-low", Efforts: []string{"low", "high"}}}}}
	cs, why := d.selectCandidates(Options{Tier: adapter.TierEasy, Filters: store.Filters{Model: "gemini-fast", Effort: "high"}})
	if len(cs) != 1 || cs[0].Model != "gemini-fast-high" || cs[0].Effort != "high" {
		t.Fatal(cs, why)
	}
	cs, why = d.selectCandidates(Options{Tier: adapter.TierEasy, Filters: store.Filters{Model: "gemini-fast", Effort: "xhigh"}})
	if len(cs) != 0 || !strings.Contains(strings.Join(why, ";"), "unsupported") {
		t.Fatal(cs, why)
	}
	a.caps.ReadOnly = false
	d.Adapters = []adapter.Adapter{a}
	cs, why = d.selectCandidates(Options{Tier: adapter.TierEasy, ReadOnly: true})
	if len(cs) != 0 || !strings.Contains(strings.Join(why, ";"), "readOnly") {
		t.Fatal(cs, why)
	}
}

func TestRelaxAfterFilteredCandidatesFail(t *testing.T) {
	d, ran, _ := routingDeps(t)
	for i, a := range d.Adapters {
		f := a.(routingFake)
		if f.fake.name == "a" {
			f.fake.err = errors.New("crash")
		}
		d.Adapters[i] = f
	}
	rec, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Filters: store.Filters{Agent: "a", Relax: true}, Prompt: "p", NoWorktree: true})
	if err != nil || rec.Status != store.Completed || rec.Agent != "b" || !strings.Contains(rec.Note, "--relax") || len(*ran) != 3 {
		t.Fatal(rec, err, *ran)
	}
}
