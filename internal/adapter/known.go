package adapter

// Cursor drives the Cursor CLI. The binary is cursor-agent: a bare `agent`
// on PATH may resolve to Grok's alias.
func Cursor() Adapter {
	return cliAdapter{
		name: "cursor", binary: "cursor-agent",
		caps:        Capabilities{SessionResume: true, StructuredOutput: true, ReadOnly: true, FileEditing: true},
		listArgs:    []string{"models"},
		parseModels: parseCursorModels,
		args: func(r Request, model string) []string {
			a := []string{"-p", "--output-format", "text", "--trust"}
			if r.ReadOnly {
				a = append(a, "--mode", "ask")
			} else {
				a = append(a, "--force", "--sandbox", "enabled")
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

// Grok drives the Grok Build CLI in single-turn mode.
func Grok() Adapter {
	return cliAdapter{
		name: "grok", binary: "grok",
		caps:        Capabilities{SessionResume: true, StructuredOutput: true, ReadOnly: true, FileEditing: true},
		listArgs:    []string{"models"},
		parseModels: parseGrokModels,
		args: func(r Request, model string) []string {
			a := []string{"--output-format", "plain"}
			if r.ReadOnly {
				a = append(a, "--permission-mode", "plan")
			} else {
				a = append(a, "--permission-mode", "acceptEdits")
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

// Antigravity drives the Antigravity CLI (agy). Effort is part of the model
// ID (e.g. gemini-3.8-flash-low). Tool permissions come from the user's
// agy settings; denied tools are surfaced as errors.
func Antigravity() Adapter {
	return cliAdapter{
		name: "antigravity", binary: "agy",
		caps:        Capabilities{SessionResume: true, StructuredOutput: true, ReadOnly: true, FileEditing: true},
		listArgs:    []string{"models"},
		parseModels: parseAgyModels,
		setDir:      true,
		args: func(r Request, model string) []string {
			a := []string{"--output-format", "text", "--sandbox", "--disable-slash-commands"}
			if r.ReadOnly {
				a = append(a, "--mode", "plan")
			} else {
				a = append(a, "--mode", "accept-edits")
			}
			if model != "" {
				a = append(a, "--model", model)
			}
			// agy requires the prompt immediately after -p, so it goes last.
			return append(a, "-p", r.Prompt)
		},
	}
}

// Known returns the adapters dispatch knows about.
func Known() []Adapter {
	return []Adapter{Codex{}, Cursor(), Grok(), Antigravity()}
}
