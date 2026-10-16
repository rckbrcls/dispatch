package adapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// defaultBinaries is the executable each agent is looked up as on PATH.
var defaultBinaries = map[string]string{
	"codex": "codex", "cursor": "cursor-agent", "grok": "grok",
	"antigravity": "agy", "opencode": "opencode",
}

// knownLocations are places a CLI is commonly installed without being on
// PATH (for example OpenCode ships its CLI inside the desktop app). "~" is
// the home directory.
var knownLocations = map[string][]string{
	"opencode": {
		"~/.opencode/bin/opencode",
		"/Applications/OpenCode.app/Contents/Resources/opencode-cli",
		"/opt/homebrew/bin/opencode",
		"/usr/local/bin/opencode",
	},
}

var (
	binMu        sync.RWMutex
	binOverrides = map[string]string{}
)

// SetBinaries replaces the per-agent executable overrides (agent name to
// path). An empty path is ignored, so callers can clear an agent by omitting it.
func SetBinaries(m map[string]string) {
	binMu.Lock()
	defer binMu.Unlock()
	binOverrides = map[string]string{}
	for k, v := range m {
		if v != "" {
			binOverrides[k] = v
		}
	}
}

// Bin is the executable to run for an agent: the configured path when set,
// otherwise its default name on PATH.
func Bin(agent string) string {
	binMu.RLock()
	defer binMu.RUnlock()
	if p, ok := binOverrides[agent]; ok {
		return p
	}
	return defaultBinaries[agent]
}

// Detection says where an agent's CLI was found on this machine.
type Detection struct {
	Agent      string `json:"agent"`
	Default    string `json:"default"`              // name looked up on PATH
	Configured string `json:"configured,omitempty"` // override from config
	Path       string `json:"path,omitempty"`       // what will actually run
	Source     string `json:"source"`               // configured | PATH | found | missing | invalid
	Suggest    string `json:"suggest,omitempty"`    // known location that exists but is not in use
}

func executable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Mode().Perm()&0o111 != 0
}

func expand(p string) string {
	if len(p) >= 2 && p[:2] == "~/" {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

// Detect reports every known agent: configured path, PATH lookup, or a
// well-known install location to offer as a suggestion.
func Detect() []Detection {
	var out []Detection
	for _, a := range Known() {
		name := a.Name()
		d := Detection{Agent: name, Default: defaultBinaries[name], Source: "missing"}
		binMu.RLock()
		d.Configured = binOverrides[name]
		binMu.RUnlock()
		switch {
		case d.Configured != "":
			if executable(d.Configured) {
				d.Path, d.Source = d.Configured, "configured"
			} else {
				d.Source = "invalid"
			}
		default:
			if p, err := exec.LookPath(d.Default); err == nil {
				d.Path, d.Source = p, "PATH"
			}
		}
		if d.Path == "" {
			for _, loc := range knownLocations[name] {
				if p := expand(loc); executable(p) {
					d.Suggest = p
					if d.Source == "missing" {
						d.Source = "found"
					}
					break
				}
			}
		}
		out = append(out, d)
	}
	return out
}

// ValidBinary checks a user-supplied path before it is saved.
func ValidBinary(p string) error {
	p = expand(p)
	if !filepath.IsAbs(p) {
		return os.ErrInvalid
	}
	if !executable(p) {
		return os.ErrNotExist
	}
	return nil
}
