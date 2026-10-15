// Package skill installs the dispatch skill into each agent's skills dir.
package skill

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed SKILL.md
var Content []byte

const Name = "dispatch"

// Target is a skills directory read by one or more agents.
type Target struct {
	Dir    string   // absolute skills directory
	Agents []string // agents that read it
}

// Targets maps agents to skills directories under home. Codex and Grok
// share ~/.agents/skills (Grok verified via `grok inspect`; Codex and
// Antigravity paths are not verified against their docs).
func Targets(home string) map[string]Target {
	shared := Target{Dir: filepath.Join(home, ".agents", "skills")}
	return map[string]Target{
		"claude":      {Dir: filepath.Join(home, ".claude", "skills")},
		"cursor":      {Dir: filepath.Join(home, ".cursor", "skills")},
		"codex":       shared,
		"grok":        shared,
		"antigravity": {Dir: filepath.Join(home, ".gemini", "config", "skills")},
	}
}

// Install writes SKILL.md into dir/dispatch/. It refuses to overwrite an
// existing skill unless force is set. It returns the written path.
func Install(dir string, force bool) (string, error) {
	path := filepath.Join(dir, Name, "SKILL.md")
	if _, err := os.Stat(path); err == nil && !force {
		return path, fmt.Errorf("%s already exists (use --force)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, Content, 0o644)
}

// Refresh rewrites the skill only where dispatch already installed it, so an
// update never creates new skill files. It returns the paths rewritten.
func Refresh(home string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, t := range Targets(home) {
		if seen[t.Dir] {
			continue
		}
		seen[t.Dir] = true
		path := filepath.Join(t.Dir, Name, "SKILL.md")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.WriteFile(path, Content, 0o644); err != nil {
			return out, err
		}
		out = append(out, path)
	}
	return out, nil
}
