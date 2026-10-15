package adapter

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// cliAdapter is a one-shot print-mode CLI: run a prompt, read stdout.
// Quota has no readable source for these CLIs; the router treats them as
// unknown and reacts to rate-limit errors instead.
type cliAdapter struct {
	name, binary string
	caps         Capabilities
	listArgs     []string
	parseModels  func(out string) []string
	// args builds the full argument list for a run.
	args func(req Request, model string) []string
	// setDir is true when the CLI has no cwd flag and must run with cmd.Dir.
	setDir bool
}

func (c cliAdapter) Name() string               { return c.name }
func (c cliAdapter) Binary() string             { return c.binary }
func (c cliAdapter) Capabilities() Capabilities { return c.caps }

func (c cliAdapter) Quota(context.Context) (Quota, error) { return Quota{}, ErrNoQuota }

// claudeModel matches Anthropic model IDs. Routing Claude models through
// other vendors' CLIs is refused by policy.
var claudeModel = regexp.MustCompile(`(?i)claude|sonnet|opus|haiku|fable`)

func (c cliAdapter) Models(ctx context.Context) ([]Model, error) {
	out, err := exec.CommandContext(ctx, c.binary, c.listArgs...).Output()
	if err != nil {
		return nil, err
	}
	var models []Model
	for _, id := range c.parseModels(string(out)) {
		if !claudeModel.MatchString(id) {
			models = append(models, Model{ID: id})
		}
	}
	return models, nil
}

func (c cliAdapter) Run(ctx context.Context, req Request, model string) (Result, error) {
	if claudeModel.MatchString(model) {
		return Result{}, fmt.Errorf("%s: claude models are not routed through this CLI", c.name)
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.binary, c.args(req, model)...)
	ownGroup(cmd)
	if c.setDir {
		cmd.Dir = req.Dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	res := Result{Output: strings.TrimSpace(stdout.String()), ExitCode: cmd.ProcessState.ExitCode()}
	switch {
	case ctx.Err() != nil:
		return res, ctx.Err()
	case runErr != nil:
		return res, fmt.Errorf("%s: %v: %s", c.name, runErr, bytes.TrimSpace(stderr.Bytes()))
	case res.Output == "" && strings.Contains(stderr.String(), "auto-denied"):
		// agy exits 0 with an empty response when headless permissions deny a tool.
		return res, fmt.Errorf("%s: permission denied in headless mode: %s", c.name, bytes.TrimSpace(stderr.Bytes()))
	}
	return res, nil
}

// --- model list parsers ---

func lines(s string) []string { return strings.Split(s, "\n") }

// "id - Display name" (cursor-agent models)
func parseCursorModels(out string) []string {
	var ids []string
	for _, l := range lines(out) {
		id, _, ok := strings.Cut(l, " - ")
		if id = strings.TrimSpace(id); ok && id != "" && !strings.Contains(id, " ") {
			ids = append(ids, id)
		}
	}
	return ids
}

// "id<TAB>Display name" (agy models)
func parseAgyModels(out string) []string {
	var ids []string
	for _, l := range lines(out) {
		if id, _, ok := strings.Cut(l, "\t"); ok && strings.TrimSpace(id) != "" {
			ids = append(ids, strings.TrimSpace(id))
		}
	}
	return ids
}

// "* id (default)" / "- id" (grok models)
func parseGrokModels(out string) []string {
	var ids []string
	for _, l := range lines(out) {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "* ") && !strings.HasPrefix(l, "- ") {
			continue
		}
		if f := strings.Fields(l[2:]); len(f) > 0 {
			ids = append(ids, f[0])
		}
	}
	return ids
}
