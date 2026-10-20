// Package integration wires dispatch into the callers that delegate to it:
// the enabled flag, the Claude Code SessionStart hook and the skill copies.
// The CLI (`on`, `off`, `status`) and the TUI share it.
package integration

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/hook"
	"dispatch/internal/skill"
)

// SkillState is one skills directory and whether dispatch's skill is in it.
type SkillState struct {
	Name      string // caller that reads it (claude, cursor, codex...)
	Dir       string
	Installed bool
	Present   bool // the caller's CLI is on PATH (claude is always assumed)
}

// State is everything `dispatch status` and the TUI need to explain why a
// session does or does not see dispatch.
type State struct {
	Enabled      bool
	SettingsPath string
	HookFound    bool
	HookCommand  string // command registered in settings, empty when none
	HookProblem  string // why the registered command cannot work, empty when fine
	Skills       []SkillState
}

// Inspect reads the current state. home is the dispatch home, userHome the
// user's home directory.
func Inspect(home, userHome string) State {
	st := State{Enabled: hook.IsEnabled(home), SettingsPath: hook.SettingsPath(userHome)}
	st.HookFound, _ = hook.IsInstalled(st.SettingsPath)
	if st.HookFound {
		st.HookCommand = hook.RegisteredCommand(st.SettingsPath)
		st.HookProblem = hookProblem(st.HookCommand)
	}
	targets := skill.Targets(userHome)
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t := targets[n]
		present := n == "claude"
		if !present {
			for _, a := range adapter.Known() {
				if a.Name() == n {
					_, err := exec.LookPath(a.Binary())
					present = err == nil
				}
			}
		}
		st.Skills = append(st.Skills, SkillState{Name: n, Dir: t.Dir, Installed: skill.IsInstalled(t.Dir), Present: present})
	}
	return st
}

// hookProblem reports why a registered hook command would fail to run: the
// first word is the binary and it must exist and be executable.
func hookProblem(cmd string) string {
	bin := firstWord(cmd)
	if bin == "" {
		return "empty command"
	}
	st, err := os.Stat(bin)
	if err != nil {
		return "binary not found: " + bin
	}
	if st.IsDir() || st.Mode().Perm()&0o111 == 0 {
		return "not executable: " + bin
	}
	return ""
}

// firstWord returns the leading shell word of cmd, honouring the single
// quotes hook.HookCommand adds around unsafe paths.
func firstWord(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, "'") {
		var b strings.Builder
		i := 1
		for i < len(cmd) {
			if cmd[i] == '\'' {
				if strings.HasPrefix(cmd[i:], `'\''`) {
					b.WriteByte('\'')
					i += 4
					continue
				}
				break
			}
			b.WriteByte(cmd[i])
			i++
		}
		return b.String()
	}
	if i := strings.IndexAny(cmd, " \t"); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

// Enable turns dispatch on: the flag, the skill where a caller exists and
// the hook. It returns one line per change.
func Enable(home, userHome, exe string) ([]string, error) {
	var out []string
	if err := hook.SetEnabled(home, true); err != nil {
		return out, err
	}
	done := map[string]bool{}
	for _, s := range Inspect(home, userHome).Skills {
		if !s.Present || s.Installed || done[s.Dir] {
			continue
		}
		done[s.Dir] = true
		p, err := skill.Install(s.Dir, false)
		if err != nil {
			return out, err
		}
		out = append(out, fmt.Sprintf("+ %-12s %s", s.Name, p))
	}
	l, err := InstallHook(userHome, exe)
	return append(out, l...), err
}

// Disable turns the context off. The hook stays registered; it prints
// nothing while the flag is absent.
func Disable(home string) error { return hook.SetEnabled(home, false) }

// InstallHook registers (or repairs) the SessionStart hook for exe.
func InstallHook(userHome, exe string) ([]string, error) {
	abs, err := filepath.Abs(exe)
	if err != nil {
		return nil, err
	}
	path := hook.SettingsPath(userHome)
	if err := hook.Install(path, abs); err != nil {
		return nil, err
	}
	return []string{"+ hook         " + path}, nil
}

// RemoveHook unregisters the SessionStart hook.
func RemoveHook(userHome string) error { return hook.Uninstall(hook.SettingsPath(userHome)) }

// InstallSkill writes (force: rewrites) the skill for one caller.
func InstallSkill(userHome, name string, force bool) (string, error) {
	t, ok := skill.Targets(userHome)[name]
	if !ok {
		return "", errors.New("unknown skill target " + name)
	}
	return skill.Install(t.Dir, force)
}

// TestHook runs the registered hook command the way Claude Code does and
// returns what it printed. Empty output with a nil error means the hook ran
// but dispatch is off.
func TestHook(command string) (string, error) {
	if command == "" {
		return "", errors.New("no hook registered")
	}
	c := exec.Command("sh", "-c", command)
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = c.Output(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Duration(hook.DefaultTimeout) * time.Second):
		_ = c.Process.Kill()
		return "", fmt.Errorf("hook timed out after %ds", hook.DefaultTimeout)
	}
	return strings.TrimSpace(string(out)), err
}
