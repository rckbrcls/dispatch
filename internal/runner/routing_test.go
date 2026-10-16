package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/config"
	"dispatch/internal/pace"
	"dispatch/internal/router"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
)

type routingFake struct {
	fake
	caps     adapter.Capabilities
	quota    adapter.Quota
	requests *[]adapter.Request
	calls    *int
	models   []adapter.Model
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
func (f routingFake) Models(context.Context) ([]adapter.Model, error) { return f.models, nil }
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

func priorityDeps(t *testing.T) Deps {
	t.Helper()
	d, _, _ := routingDeps(t)
	for i, a := range d.Adapters {
		f := a.(routingFake)
		f.quota = adapter.Quota{}
		f.caps = adapter.Capabilities{ReadOnly: true, FileEditing: true}
		d.Adapters[i] = f
	}
	d.Table = tiers.Table{Tiers: map[adapter.Tier][]tiers.Entry{
		adapter.TierNormal: {
			{Agent: "a", Model: "fast", Provider: "openai", Speed: 5, Strength: 1},
			{Agent: "b", Model: "strong", Provider: "google", Speed: 1, Strength: 5},
		},
	}}
	return d
}

func TestPriorityRoutesDifferentModels(t *testing.T) {
	d := priorityDeps(t)
	route := func(priority string) RouteReport {
		t.Helper()
		r, err := d.Route(context.Background(), Options{Tier: adapter.TierNormal, Filters: store.Filters{Priority: priority}})
		if err != nil || r.Winner == nil || len(r.Candidates) != 2 {
			t.Fatalf("%s: %+v %v", priority, r, err)
		}
		return r
	}
	speed := route(router.PrioritySpeed)
	quality := route(router.PriorityQuality)
	balanced := route(router.PriorityBalanced)
	if speed.Winner.Agent != "a" || speed.Winner.Model != "fast" || !strings.Contains(speed.PriorityNote, "stays") {
		t.Fatalf("speed %+v %s", speed.Winner, speed.PriorityNote)
	}
	if !strings.Contains(speed.Candidates[0].Reason, "paceSlack=") || !strings.Contains(speed.Candidates[0].Reason, "duration=") {
		t.Fatal(speed.Candidates[0].Reason)
	}
	if quality.Winner.Agent != "b" || quality.Winner.Model != "strong" || !strings.Contains(quality.PriorityNote, "moved from") || !strings.Contains(quality.PriorityNote, "a/fast") {
		t.Fatalf("quality %+v %s", quality.Winner, quality.PriorityNote)
	}
	if balanced.Winner.Agent != speed.Winner.Agent || balanced.Winner.Model != speed.Winner.Model || !strings.Contains(balanced.PriorityNote, "unchanged") {
		t.Fatalf("balanced %+v %s", balanced.Winner, balanced.PriorityNote)
	}
	_, rejS := d.selectCandidates(Options{Tier: adapter.TierNormal, Filters: store.Filters{Priority: router.PrioritySpeed}})
	_, rejQ := d.selectCandidates(Options{Tier: adapter.TierNormal, Filters: store.Filters{Priority: router.PriorityQuality}})
	if strings.Join(rejS, "\n") != strings.Join(rejQ, "\n") {
		t.Fatalf("priority filtered\n%v\n%v", rejS, rejQ)
	}
	rec, err := d.Run(context.Background(), Options{Tier: adapter.TierNormal, Filters: store.Filters{Priority: router.PriorityQuality}, Prompt: "p", NoWorktree: true})
	if err != nil || rec.Agent != "b" || rec.Model != "strong" {
		t.Fatal(rec, err)
	}
	if rec.Requested == nil || rec.Requested.Priority != router.PriorityQuality || rec.Resolved == nil || rec.Resolved.Priority != router.PriorityQuality {
		t.Fatalf("record %+v %+v", rec.Requested, rec.Resolved)
	}
	sum := Summarize(rec)
	if sum.Requested.Priority != router.PriorityQuality || sum.Resolved.Priority != router.PriorityQuality {
		t.Fatalf("summary %+v %+v", sum.Requested, sum.Resolved)
	}
}

func TestMeasuredEvidenceOverridesPriorAfterMinSamples(t *testing.T) {
	d := priorityDeps(t)
	entries := d.Table.Tiers[adapter.TierNormal]
	entries[0].Speed = 1
	entries[1].Speed = 5
	d.Table.Tiers[adapter.TierNormal] = entries
	n := 0
	seed := func(tier, model string, count int, ms int64) {
		t.Helper()
		for i := 0; i < count; i++ {
			n++
			err := d.Store.Append(store.Record{
				ID: fmt.Sprintf("e-%d", n), Status: store.Completed, Tier: tier, Model: model,
				RunMs: store.I64(ms), CreatedAt: d.Now(), FinishedAt: d.Now(),
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("hard", "fast", 8, 1000)
	winner := func() string {
		t.Helper()
		r, err := d.Route(context.Background(), Options{Tier: adapter.TierNormal, Filters: store.Filters{Priority: router.PrioritySpeed}})
		if err != nil || r.Winner == nil {
			t.Fatal(err, r)
		}
		return r.Winner.Model
	}
	if winner() != "strong" {
		t.Fatal("hard-tier samples changed the easy/normal prior")
	}
	seed("normal", "fast", 4, 1000)
	seed("normal", "strong", 4, 600000)
	if winner() != "strong" {
		t.Fatal("prior overridden before min samples")
	}
	seed("normal", "fast", 1, 1000)
	seed("normal", "strong", 1, 600000)
	r, err := d.Route(context.Background(), Options{Tier: adapter.TierNormal, Filters: store.Filters{Priority: router.PrioritySpeed}})
	if err != nil || r.Winner == nil || r.Winner.Model != "fast" || !strings.Contains(r.Candidates[0].Reason, "blended") {
		t.Fatalf("%+v %v", r.Winner, r.Candidates)
	}
}

func TestPriorityDoesNotBypassConstraints(t *testing.T) {
	d := priorityDeps(t)
	if err := d.Store.SetCooldown("a", d.Now().Add(time.Hour), d.Now()); err != nil {
		t.Fatal(err)
	}
	cs, why := d.selectCandidates(Options{Tier: adapter.TierNormal, Filters: store.Filters{Priority: router.PrioritySpeed}})
	if len(cs) != 1 || cs[0].Adapter.Name() != "b" || !strings.Contains(strings.Join(why, ";"), "cooldown") {
		t.Fatal(cs, why)
	}
	d = priorityDeps(t)
	cs, why = d.selectCandidates(Options{Tier: adapter.TierNormal, Filters: store.Filters{Priority: router.PrioritySpeed, Provider: "google"}})
	if len(cs) != 1 || cs[0].Model != "strong" || !strings.Contains(strings.Join(why, ";"), "provider filter") {
		t.Fatal(cs, why)
	}
	a := d.Adapters[0].(routingFake)
	a.caps.ReadOnly = false
	d.Adapters[0] = a
	row := d.Table.Tiers[adapter.TierNormal]
	row[0].Strength = 5
	row[1].Strength = 1
	d.Table.Tiers[adapter.TierNormal] = row
	cs, why = d.selectCandidates(Options{Tier: adapter.TierNormal, ReadOnly: true, Filters: store.Filters{Priority: router.PriorityQuality}})
	if len(cs) != 1 || cs[0].Adapter.Name() != "b" || !strings.Contains(strings.Join(why, ";"), "readOnly") {
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

func TestPersistentPolicyAppliesToFallbackRelaxAndFlags(t *testing.T) {
	d, ran, _ := routingDeps(t)
	d.Policy.Allow.Providers = []string{"openai"}
	d.Policy.Deny.Models = []string{"gpt-small"}
	rec, _ := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Filters: store.Filters{Relax: true}, Prompt: "p", NoWorktree: true})
	if rec.Status != store.Failed || len(*ran) != 0 || !strings.Contains(rec.Error, "policy deny.models") {
		t.Fatalf("%+v %v", rec, *ran)
	}
	d.Policy.Deny = config.Rules{}
	cs, why := d.selectCandidates(Options{Tier: adapter.TierEasy, Filters: store.Filters{Provider: "google", Relax: true}})
	if len(cs) != 0 || !strings.Contains(strings.Join(why, ";"), "policy allow.providers") {
		t.Fatalf("%v %v", cs, why)
	}
	rec, _ = d.Run(context.Background(), Options{Tier: adapter.TierEasy, Filters: store.Filters{Provider: "google", Relax: true}, Prompt: "p", NoWorktree: true})
	if rec.Status != store.Failed || !strings.Contains(rec.Error, "policy allow.providers") {
		t.Fatalf("%+v", rec)
	}
}

func TestDiscoverAddsNormalCandidateOnlyWhenEnabled(t *testing.T) {
	d, _, _ := routingDeps(t)
	a := d.Adapters[0].(routingFake)
	a.models = []adapter.Model{{ID: "gpt-discovered"}}
	d.Adapters[0] = a
	cs, _ := d.selectCandidatesWith(context.Background(), Options{Tier: adapter.TierNormal})
	if len(cs) != 0 {
		t.Fatal("discover default must be off", cs)
	}
	d.Policy.Discover = true
	cs, why := d.selectCandidatesWith(context.Background(), Options{Tier: adapter.TierNormal})
	if len(cs) != 1 || cs[0].Model != "gpt-discovered" || cs[0].Note != "discovered" {
		t.Fatal(cs, why)
	}
}
