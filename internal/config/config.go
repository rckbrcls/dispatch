// Package config reads the optional dispatch config file.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// File is ~/.dispatch/config.json. Unknown keys are ignored.
type File struct {
	Priority string `json:"priority,omitempty"`
	Allow    Rules  `json:"allow,omitempty"`
	Deny     Rules  `json:"deny,omitempty"`
	Discover bool   `json:"discover,omitempty"`
}

// Rules is the persistent routing policy. An empty allow dimension does not
// restrict that dimension; deny always takes precedence.
type Rules struct {
	Providers []string `json:"providers,omitempty"`
	Models    []string `json:"models,omitempty"`
	Agents    []string `json:"agents,omitempty"`
}

// Path is the config file inside a dispatch home directory.
func Path(home string) string { return filepath.Join(home, "config.json") }

// Load reads path. A missing or empty file is a zero config, not an error.
func Load(path string) (File, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return File{}, nil
	}
	f, _, err := decode(path, b)
	return f, err
}

// Validate checks values assembled by callers before writing them.
func Validate(f File) error {
	switch f.Priority {
	case "", "speed", "balanced", "quality":
		return nil
	default:
		return fmt.Errorf("invalid priority %q (want speed|balanced|quality)", f.Priority)
	}
}

type present struct{ priority, allow, deny, discover bool }

// LoadAll merges global and project config. Project values win by key; deny
// lists are unioned so a project cannot accidentally weaken a global deny.
func LoadAll(global, project string) (File, error) {
	g, gp, err := loadPresent(global)
	if err != nil {
		return File{}, err
	}
	p, pp, err := loadPresent(project)
	if err != nil {
		return File{}, err
	}
	out := g
	if pp.priority {
		out.Priority = p.Priority
	}
	if pp.discover {
		out.Discover = p.Discover
	}
	if pp.allow {
		out.Allow = p.Allow
	}
	if pp.deny {
		out.Deny = union(g.Deny, p.Deny)
	}
	_ = gp
	return out, nil
}

func loadPresent(path string) (File, present, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return File{}, present{}, nil
	}
	if err != nil {
		return File{}, present{}, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return File{}, present{}, nil
	}
	return decode(path, b)
}

func decode(path string, b []byte) (File, present, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return File{}, present{}, fmt.Errorf("%s: %w", path, err)
	}
	if raw == nil {
		return File{}, present{}, fmt.Errorf("%s: config must be an object", path)
	}
	var f File
	var p present
	if v, ok := raw["priority"]; ok {
		p.priority = true
		if isNull(v) {
			return File{}, p, fmt.Errorf("%s: priority must be a string", path)
		}
		if err := json.Unmarshal(v, &f.Priority); err != nil {
			return File{}, p, fmt.Errorf("%s: priority must be a string", path)
		}
	}
	if v, ok := raw["discover"]; ok {
		p.discover = true
		if isNull(v) {
			return File{}, p, fmt.Errorf("%s: discover must be a boolean", path)
		}
		if err := json.Unmarshal(v, &f.Discover); err != nil {
			return File{}, p, fmt.Errorf("%s: discover must be a boolean", path)
		}
	}
	if v, ok := raw["allow"]; ok {
		p.allow = true
		var err error
		if f.Allow, err = decodeRules(v); err != nil {
			return File{}, p, fmt.Errorf("%s: allow %w", path, err)
		}
	}
	if v, ok := raw["deny"]; ok {
		p.deny = true
		var err error
		if f.Deny, err = decodeRules(v); err != nil {
			return File{}, p, fmt.Errorf("%s: deny %w", path, err)
		}
	}
	switch f.Priority {
	case "", "speed", "balanced", "quality":
	default:
		return File{}, p, fmt.Errorf("%s: invalid priority %q (want speed|balanced|quality)", path, f.Priority)
	}
	return f, p, nil
}

func decodeRules(v json.RawMessage) (Rules, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(v, &raw); err != nil || raw == nil {
		return Rules{}, fmt.Errorf("must be an object")
	}
	var r Rules
	for key, dst := range map[string]*[]string{"providers": &r.Providers, "models": &r.Models, "agents": &r.Agents} {
		if v, ok := raw[key]; ok {
			if isNull(v) {
				return Rules{}, fmt.Errorf("%s must be an array of strings", key)
			}
			if err := json.Unmarshal(v, dst); err != nil {
				return Rules{}, fmt.Errorf("%s must be an array of strings", key)
			}
		}
	}
	return r, nil
}

func isNull(v json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(v), []byte("null")) }

func union(a, b Rules) Rules {
	return Rules{Providers: unionStrings(a.Providers, b.Providers), Models: unionStrings(a.Models, b.Models), Agents: unionStrings(a.Agents, b.Agents)}
}
func unionStrings(a, b []string) []string {
	out := append([]string(nil), a...)
	for _, v := range b {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// PolicyReason explains why a provider/model/agent is excluded, or returns
// empty when it is permitted. Deny is deliberately checked first.
func PolicyReason(f File, provider, model, agent string) string {
	if slices.Contains(f.Deny.Providers, provider) {
		return "policy deny.providers includes " + provider
	}
	if slices.Contains(f.Deny.Models, model) {
		return "policy deny.models includes " + model
	}
	if slices.Contains(f.Deny.Agents, agent) {
		return "policy deny.agents includes " + agent
	}
	if len(f.Allow.Providers) > 0 && !slices.Contains(f.Allow.Providers, provider) {
		return "policy allow.providers requires one of " + join(f.Allow.Providers)
	}
	if len(f.Allow.Models) > 0 && !slices.Contains(f.Allow.Models, model) {
		return "policy allow.models requires one of " + join(f.Allow.Models)
	}
	if len(f.Allow.Agents) > 0 && !slices.Contains(f.Allow.Agents, agent) {
		return "policy allow.agents requires one of " + join(f.Allow.Agents)
	}
	return ""
}
func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}

// Write atomically updates known keys while retaining unknown JSON keys.
func Write(path string, f File) error {
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
			return fmt.Errorf("%s: invalid config", path)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	put := func(k string, v any) error {
		b, err := json.Marshal(v)
		if err == nil {
			raw[k] = b
		}
		return err
	}
	if err := put("priority", f.Priority); err != nil {
		return err
	}
	if err := put("discover", f.Discover); err != nil {
		return err
	}
	if err := put("allow", f.Allow); err != nil {
		return err
	}
	if err := put("deny", f.Deny); err != nil {
		return err
	}
	return writeRaw(path, raw)
}

// WriteKey atomically replaces one known key and retains every other key.
// It is used by the CLI so changing project allow does not accidentally
// override an inherited priority or discover value.
func WriteKey(path, key string, value any) error {
	raw := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
			return fmt.Errorf("%s: invalid config", path)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw[key] = b
	return writeRaw(path, raw)
}

func writeRaw(path string, raw map[string]json.RawMessage) error {
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(b); err == nil {
		err = tmp.Chmod(0o600)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
