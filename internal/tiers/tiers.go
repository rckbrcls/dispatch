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
	Agent string `json:"agent"`
	Model string `json:"model"`
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
		for _, e := range es {
			if e.Agent == "" || e.Model == "" {
				return Table{}, fmt.Errorf("tier %q: entries need agent and model", tier)
			}
		}
	}
	return t, nil
}
