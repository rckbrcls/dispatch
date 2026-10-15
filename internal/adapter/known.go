package adapter

import (
	"context"
	"os/exec"
	"strings"
)

// Cursor drives the Cursor CLI. The binary is cursor-agent: a bare `agent`
// on PATH may resolve to Grok's alias. Sessions are pre-allocated with
// `create-chat` and selected with --resume.
func Cursor() Adapter {
	return cliAdapter{
		name: "cursor", binary: "cursor-agent",
		caps:        Capabilities{SessionResume: true, StructuredOutput: true, ReadOnly: true, FileEditing: true},
		listArgs:    []string{"models"},
		parseModels: parseCursorModels,
		newSession: func(ctx context.Context) (string, error) {
			out, err := exec.CommandContext(ctx, "cursor-agent", "create-chat").Output()
			return strings.TrimSpace(string(out)), err
		},
		args: func(r Request, model, session string, _ bool) []string {
			a := []string{"-p", "--output-format", "text", "--trust"}
			if r.ReadOnly {
				a = append(a, "--mode", "ask")
			} else {
				a = append(a, "--force", "--sandbox", "enabled")
			}
			if session != "" {
				a = append(a, "--resume", session)
			}
			if model != "" {
				a = append(a, "--model", model)
			}
			if r.Dir != "" {
				a = append(a, "--workspace", r.Dir)
			}
			return append(a, r.Prompt)
		},
	}
}

// Grok drives the Grok Build CLI in single-turn mode. A fresh run is given
// a caller-chosen session UUID (-s) so it can be resumed with --resume.
func Grok() Adapter {
	return cliAdapter{
		name: "grok", binary: "grok",
		caps:        Capabilities{SessionResume: true, StructuredOutput: true, ReadOnly: true, FileEditing: true},
		listArgs:    []string{"models"},
		parseModels: parseGrokModels,
		newSession:  func(context.Context) (string, error) { return newUUID(), nil },
		args: func(r Request, model, session string, resume bool) []string {
			a := []string{"--output-format", "plain"}
			if r.ReadOnly {
				a = append(a, "--permission-mode", "plan")
			} else {
				a = append(a, "--permission-mode", "acceptEdits")
			}
			if resume {
				a = append(a, "--resume", session)
			} else if session != "" {
				a = append(a, "-s", session)
			}
			if model != "" {
				a = append(a, "--model", model)
			}
			if r.Dir != "" {
				a = append(a, "--cwd", r.Dir)
			}
			// -p takes the prompt as its value.
			return append(a, "-p", r.Prompt)
		},
	}
}

// Known returns the adapters dispatch knows about.
func Known() []Adapter {
	return []Adapter{Codex{}, Cursor(), Grok(), Antigravity{}}
}
