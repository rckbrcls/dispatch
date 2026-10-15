package runner

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/router"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
)

func validateOptions(o Options) error {
	if o.Tier != "" && !o.Tier.Valid() {
		return fmt.Errorf("invalid tier %q", o.Tier)
	}
	if o.Tier == "" && o.Filters.Model == "" && o.Filters.Provider == "" {
		return fmt.Errorf("tier required without --model or --provider")
	}
	if o.Filters.Effort != "" && !adapter.ValidEffort(o.Filters.Effort) {
		return fmt.Errorf("invalid effort %q (want low|medium|high|xhigh)", o.Filters.Effort)
	}
	if o.Filters.Effort != "" && adapter.EffortOf(o.Filters.Model) != "" && adapter.EffortOf(o.Filters.Model) != o.Filters.Effort {
		return fmt.Errorf("model suffix conflicts with requested effort")
	}
	return nil
}

// selectCandidates applies hard constraints before quota/model ranking. In
// particular a fixed model never competes with a different model in Rank.
func (d Deps) selectCandidates(o Options) ([]router.Candidate, []string) {
	f := o.Filters
	var entries []tiers.Entry
	var rejected []string
	all := []tiers.Entry{}
	for _, t := range []adapter.Tier{adapter.TierEasy, adapter.TierNormal, adapter.TierHard} {
		all = append(all, d.Table.Tiers[t]...)
		if o.Tier == "" || o.Tier == t {
			entries = append(entries, d.Table.Tiers[t]...)
		} else {
			for _, e := range d.Table.Tiers[t] {
				rejected = append(rejected, e.Agent+"/"+e.Model+": tier filter requires "+string(o.Tier))
			}
		}
	}
	found := false
	for _, e := range all {
		if matchesModel(e.Model, f.Model) {
			found = true
		}
	}
	outside := f.Model != "" && !found
	if outside {
		// An unlisted model can use an explicitly named agent, or agents with
		// a declared model of the same provider. Never guess cross-provider access.
		entries = nil
		for _, a := range d.Adapters {
			desiredProvider := adapter.ProviderOf("", f.Model)
			if desiredProvider == "" {
				desiredProvider = f.Provider
			}
			if desiredProvider == "" {
				entries = append(entries, tiers.Entry{Agent: a.Name(), Model: f.Model})
				continue
			}
			if f.Agent != "" && a.Name() == f.Agent {
				entries = append(entries, tiers.Entry{Agent: a.Name(), Model: f.Model})
				continue
			}
			if adapter.ProviderOf(a.Name(), "") == desiredProvider {
				entries = append(entries, tiers.Entry{Agent: a.Name(), Model: f.Model, Provider: desiredProvider})
				continue
			}
			for _, e := range all {
				provider := e.Provider
				if provider == "" {
					provider = adapter.ProviderOf(e.Agent, e.Model)
				}
				if e.Agent == a.Name() && provider == desiredProvider && provider != "" {
					entries = append(entries, tiers.Entry{Agent: a.Name(), Model: f.Model, Provider: provider})
					break
				}
			}
		}
	}
	var cs []router.Candidate
	seen := map[string]bool{}
	cool := d.Store.Cooldowns(d.now())
	for _, e := range entries {
		label := e.Agent + "/" + e.Model
		reject := func(reason string) { rejected = append(rejected, label+": "+reason) }
		provider := e.Provider
		if provider == "" {
			provider = adapter.ProviderOf(e.Agent, e.Model)
		}
		switch {
		case f.Provider != "" && f.Provider != provider:
			reject("provider filter requires " + f.Provider)
			continue
		case f.Model != "" && !matchesModel(e.Model, f.Model):
			reject("model filter requires " + f.Model)
			continue
		case f.Agent != "" && f.Agent != e.Agent:
			reject("agent filter requires " + f.Agent)
			continue
		}
		a := d.adapter(e.Agent)
		if a == nil || !d.installed(a.Binary()) {
			reject("agent unavailable")
			continue
		}
		if !adapter.AllowedModel(e.Model) {
			reject("Claude models refused in third-party CLIs")
			continue
		}
		if _, ok := cool[e.Agent]; ok {
			reject("agent in cooldown")
			continue
		}
		caps := a.Capabilities()
		if o.ReadOnly && !caps.ReadOnly {
			reject("does not support readOnly")
			continue
		}
		if !o.ReadOnly && !caps.FileEditing {
			reject("does not support fileEditing")
			continue
		}
		model, effort := e.Model, f.Effort
		if f.Model != "" && adapter.EffortOf(f.Model) != "" {
			model = f.Model
			if effort == "" {
				effort = adapter.EffortOf(f.Model)
			}
		}
		explicitEffort := effort != ""
		if effort == "" {
			effort = adapter.EffortOf(model)
		}
		if effort == "" && caps.EffortMode != "" {
			switch o.Tier {
			case adapter.TierEasy:
				effort = "low"
			case adapter.TierHard:
				effort = "high"
			default:
				effort = "medium"
			}
		}
		if effort != "" {
			supported := e.Efforts
			if len(supported) == 0 {
				supported = caps.Efforts
			}
			if len(supported) == 0 && adapter.EffortOf(e.Model) != "" {
				supported = []string{adapter.EffortOf(e.Model)}
			}
			if explicitEffort && caps.EffortMode == "" {
				// A listed immutable model suffix is the only supported control for
				// CLIs without an independent reasoning setting.
				if adapter.EffortOf(model) != effort {
					reject("effort unsupported by agent (no effort control)")
					continue
				}
			}
			if len(supported) == 0 || !slices.Contains(supported, effort) || (len(caps.Efforts) > 0 && !slices.Contains(caps.Efforts, effort)) {
				reject("effort " + effort + " unsupported by model/agent")
				continue
			}
			switch caps.EffortMode {
			case "model-suffix":
				model = adapter.BaseModel(model) + "-" + effort
			case "config":
			default:
				if adapter.EffortOf(model) != effort {
					reject("effort unsupported by agent")
					continue
				}
			}
		}
		key := e.Agent + "/" + model + "/" + effort
		if seen[key] {
			continue
		}
		seen[key] = true
		c := router.Candidate{Adapter: a, Model: model, Provider: provider, Effort: effort}
		if outside {
			c.Note = "explicit model outside tier table"
		}
		if slices.Contains(o.Prefer, e.Agent) {
			c.Bias = router.PreferBias
		}
		if slices.Contains(o.Avoid, e.Agent) {
			c.Bias += router.AvoidBias
		}
		cs = append(cs, c)
	}
	if len(cs) == 0 && len(rejected) == 0 {
		rejected = append(rejected, "no candidate matches tier/model intersection")
	}
	return cs, rejected
}

func matchesModel(model, requested string) bool {
	if requested == "" {
		return false
	}
	if model == requested {
		return true
	}
	return adapter.BaseModel(model) == adapter.BaseModel(requested)
}

type RouteCandidate struct {
	store.Filters
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// RouteReport contains only structured routing decisions, never agent output.
type RouteReport struct {
	Requested  store.Filters    `json:"requested"`
	Candidates []RouteCandidate `json:"candidates"`
	Eliminated []string         `json:"eliminated"`
	Winner     *store.Filters   `json:"winner,omitempty"`
	Reason     string           `json:"reason"`
	Note       string           `json:"note,omitempty"`
}

// Route reads cached quota and cooldown state. It never invokes a CLI,
// records a snapshot/run, reconciles processes, or creates a worktree.
func (d Deps) Route(ctx context.Context, o Options) (RouteReport, error) {
	r := RouteReport{Requested: o.Filters, Candidates: []RouteCandidate{}}
	if err := validateOptions(o); err != nil {
		return r, err
	}
	cs, rejected := d.selectCandidates(o)
	r.Eliminated = rejected
	if len(cs) == 0 && o.Filters.Relax {
		o.Filters = store.Filters{}
		if o.Tier == "" {
			o.Tier = adapter.TierNormal
		}
		cs, rejected = d.selectCandidates(o)
		r.Eliminated = append(r.Eliminated, rejected...)
		r.Note = "--relax: filters relaxed to tier routing"
	}
	for _, c := range d.evaluate(ctx, cs, false) {
		r.Candidates = append(r.Candidates, RouteCandidate{Filters: store.Filters{Provider: c.Provider, Model: c.Model, Effort: c.Effort, Agent: c.Adapter.Name()}, Score: router.Score(c), Reason: router.Reason(c)})
		if c.Note != "" {
			r.Note = joinNote(r.Note, c.Note)
		}
	}
	if len(r.Candidates) == 0 {
		r.Reason = "no eligible candidate: " + strings.Join(r.Eliminated, "; ")
		return r, nil
	}
	r.Winner = &r.Candidates[0].Filters
	r.Reason = "lowest score wins; stable table order breaks ties; cached quota only: " + r.Candidates[0].Reason
	return r, nil
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}
