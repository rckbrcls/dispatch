// Package subagent loads named delegation profiles: a role (instructions,
// tier, read-only, timeout) plus soft agent preferences for the router.
package subagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/router"
)

type Profile struct {
	Provider     string   `json:"provider,omitempty"`
	Model        string   `json:"model,omitempty"`
	Effort       string   `json:"effort,omitempty"`
	Agent        string   `json:"agent,omitempty"`
	Relax        bool     `json:"relax,omitempty"`
	Priority     string   `json:"priority,omitempty"`
	Tier         string   `json:"tier,omitempty"`
	ReadOnly     bool     `json:"readonly,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Prefer       []string `json:"prefer,omitempty"`
	Avoid        []string `json:"avoid,omitempty"`
	Timeout      string   `json:"timeout,omitempty"`
	NoVerify     bool     `json:"noVerify,omitempty"`
}

type File struct {
	Agents map[string]Profile `json:"agents"`
}

// Example is written by `dispatch subagents --init`.
const Example = `{
  "agents": {
    "reviewer": {
      "tier": "hard",
      "readonly": true,
      "instructions": "Revise o diff. Aponte só bugs reais, com arquivo e linha.",
      "timeout": "15m"
    },
    "tester": {
      "tier": "normal",
      "instructions": "Escreva e rode testes para a mudança descrita."
    }
  }
}
`

// Load reads path; a missing file is an empty set.
func Load(path string) (map[string]Profile, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Agents, nil
}

// LoadAll merges the user file with the project file; project wins.
func LoadAll(userPath, projectPath string) (map[string]Profile, error) {
	out := map[string]Profile{}
	for _, p := range []string{userPath, projectPath} {
		m, err := Load(p)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			out[k] = v
		}
	}
	return out, nil
}

// ProjectPath is where a project keeps its profiles.
func ProjectPath(dir string) string { return filepath.Join(dir, ".dispatch", "agents.json") }

// Validate reports problems with one profile. known holds agent names.
func Validate(name string, p Profile, known map[string]bool) []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, name+": "+fmt.Sprintf(f, a...)) }
	if p.Effort != "" && !adapter.ValidEffort(p.Effort) {
		add("invalid effort %q", p.Effort)
	}
	if p.Priority != "" && !router.ValidPriority(p.Priority) {
		add("invalid priority %q (want speed|balanced|quality)", p.Priority)
	}
	if p.Agent != "" && !known[p.Agent] {
		add("unknown agent %q", p.Agent)
	}
	if p.Tier != "" && !adapter.Tier(p.Tier).Valid() {
		add("invalid tier %q (want easy|normal|hard)", p.Tier)
	}
	if p.Timeout != "" {
		if _, err := time.ParseDuration(p.Timeout); err != nil {
			add("invalid timeout %q", p.Timeout)
		}
	}
	for _, a := range p.Prefer {
		if !known[a] {
			add("prefer: unknown agent %q", a)
		}
	}
	for _, a := range p.Avoid {
		if !known[a] {
			add("avoid: unknown agent %q", a)
		}
	}
	for _, a := range p.Prefer {
		for _, b := range p.Avoid {
			if a == b {
				add("agent %q is both preferred and avoided", a)
			}
		}
	}
	if p.Provider == "" && p.Model == "" && p.Agent == "" && p.Effort == "" && p.Priority == "" && p.Instructions == "" && p.Tier == "" && len(p.Prefer) == 0 && len(p.Avoid) == 0 {
		add("empty profile")
	}
	return out
}

// Names returns sorted profile names.
func Names(m map[string]Profile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
