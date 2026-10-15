// Package tiers loads the declarative tier -> (agent, model) table.
// Order within a tier is preference order; the router may reorder by pace.
package tiers

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"

	"dispatch/internal/adapter"
)

//go:embed default.json
var defaultJSON []byte

type Entry struct {
	Provider string   `json:"provider,omitempty"`
	Efforts  []string `json:"efforts,omitempty"`
	Agent    string   `json:"agent"`
	Model    string   `json:"model"`
}

type Table struct {
	Tiers map[adapter.Tier][]Entry `json:"tiers"`
}

// Default returns the embedded table (placeholders; expected to be edited).
func Default() Table {
	t, err := parse(defaultJSON)
	if err != nil {
		panic(err)
	}
	return t
}

// Load reads path, or the embedded default when the file does not exist.
func Load(path string) (Table, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return Table{}, err
	}
	t, err := parse(b)
	if err != nil {
		return Table{}, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// WriteDefault writes the embedded table to path unless it already exists.
func WriteDefault(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	return os.WriteFile(path, defaultJSON, 0o644)
}

func parse(b []byte) (Table, error) {
	var t Table
	if err := json.Unmarshal(b, &t); err != nil {
		return Table{}, err
	}
	for tier, es := range t.Tiers {
		if !tier.Valid() {
			return Table{}, fmt.Errorf("unknown tier %q", tier)
		}
		for i, e := range es {
			if e.Provider == "" {
				e.Provider = adapter.ProviderOf(e.Agent, e.Model)
				es[i].Provider = e.Provider
			}
			for _, effort := range e.Efforts {
				if !adapter.ValidEffort(effort) {
					return Table{}, fmt.Errorf("tier %s: invalid effort %q", tier, effort)
				}
			}
			if derived := adapter.ProviderOf("", e.Model); e.Provider != "" && derived != "" && e.Provider != derived {
				return Table{}, fmt.Errorf("tier %s: provider %q disagrees with model %s", tier, e.Provider, e.Model)
			}
			if e.Agent == "" || e.Model == "" {
				return Table{}, fmt.Errorf("tier %q: entries need agent and model", tier)
			}
		}
	}
	return t, nil
}

// CapabilityProblems validates declared effort controls without invoking CLIs.
func (t Table) CapabilityProblems(adapters []adapter.Adapter) []string {
	var problems []string
	for tier, entries := range t.Tiers {
		for _, e := range entries {
			for _, a := range adapters {
				if a.Name() != e.Agent {
					continue
				}
				caps := a.Capabilities()
				for _, effort := range e.Efforts {
					supported := true
					if !adapter.ValidEffort(effort) {
						supported = false
					}
					switch caps.EffortMode {
					case "config":
						found := false
						for _, v := range caps.Efforts {
							if v == effort {
								found = true
							}
						}
						supported = supported && found
					case "model-suffix":
					default:
						supported = supported && adapter.EffortOf(e.Model) == effort
					}
					if !supported {
						problems = append(problems, fmt.Sprintf("%s: %s/%s: effort %s unsupported by agent", tier, e.Agent, e.Model, effort))
					}
				}
			}
		}
	}
	return problems
}
