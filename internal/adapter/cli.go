package adapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"

	"dispatch/internal/sandbox"
)

// cliAdapter is a one-shot print-mode CLI: run a prompt, read stdout.
// Quota has no readable source for these CLIs unless a subtype adds one;
// the router then treats them as unknown and learns from rate-limit errors.
type cliAdapter struct {
	name, binary string
	caps         Capabilities
	flags        []probeFlag
	listArgs     []string
	parseModels  func(out string) []string
	// newSession allocates a session id before a fresh run (optional).
	newSession func(ctx context.Context) (string, error)
	// args builds the argument list. session is the id to use or resume.
	args func(req Request, model, session string, resume bool) []string
	// parse extracts output and session id from stdout (optional).
	parse func(stdout string) (output, session string)
	// setDir is true when the CLI has no cwd flag and must run with cmd.Dir.
	setDir bool
}

func (c cliAdapter) Name() string               { return c.name }
func (c cliAdapter) Binary() string             { return Bin(c.name) }
func (c cliAdapter) Capabilities() Capabilities { return c.caps }

func (c cliAdapter) Probe(ctx context.Context) ProbeReport {
	return probeBinary(ctx, c.name, c.Binary(), c.caps, c.flags)
}

func (c cliAdapter) Resume(ctx context.Context, req Request, model string) (Result, error) {
	if req.ResumeSession == "" {
		return Result{}, fmt.Errorf("%s: resume requires a session", c.name)
	}
	return c.Run(ctx, req, model)
}

func (c cliAdapter) Quota(context.Context) (Quota, error) { return Quota{}, ErrNoQuota }

// claudeModel matches Anthropic model IDs. Routing Claude models through
// other vendors' CLIs is refused by policy.
var claudeModel = regexp.MustCompile(`(?i)claude|sonnet|opus|haiku|fable`)

func (c cliAdapter) Models(ctx context.Context) ([]Model, error) {
	cmd := exec.CommandContext(ctx, c.Binary(), c.listArgs...)
	cmd.Env = sandbox.Environ()
	out, err := cmd.Output()
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
	session, resume := req.ResumeSession, req.ResumeSession != ""
	if !resume && c.newSession != nil {
		var err error
		if session, err = c.newSession(ctx); err != nil {
			return Result{}, fmt.Errorf("%s: new session: %w", c.name, err)
		}
	}
	cmd := exec.CommandContext(ctx, c.Binary(), c.args(req, model, session, resume)...)
	if len(req.Env) > 0 {
		cmd.Env = req.Env
	} else {
		cmd.Env = sandbox.Environ()
	}
	if c.setDir {
		cmd.Dir = req.Dir
	}
	var stdout, stderr bytes.Buffer
	logOut, logErr, closeLog := openLiveLog(req.LogPath)
	defer closeLog()
	cmd.Stdout, cmd.Stderr = io.MultiWriter(&stdout, logOut), io.MultiWriter(&stderr, logErr)
	runErr := runAgent(cmd, req.Started)

	if errors.Is(runErr, ErrProcessesAlive) {
		return Result{ExitCode: -1}, runErr
	}
	res := Result{ExitCode: cmd.ProcessState.ExitCode(), SessionID: session}
	if c.parse != nil {
		var parsed string
		res.Output, parsed = c.parse(stdout.String())
		if parsed != "" {
			res.SessionID = parsed
		}
	} else {
		res.Output = strings.TrimSpace(stdout.String())
	}
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

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// parseAgyJSON reads agy's --output-format json envelope.
func parseAgyJSON(stdout string) (string, string) {
	var env struct {
		ConversationID string `json:"conversation_id"`
		Response       string `json:"response"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		return strings.TrimSpace(stdout), ""
	}
	return strings.TrimSpace(env.Response), env.ConversationID
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

// parseOpenCodeJSON reads `opencode run --format json`: one event per line;
// "text" parts carry the reply and every event carries the session id.
func parseOpenCodeJSON(stdout string) (string, string) {
	var text []string
	session := ""
	for _, l := range lines(stdout) {
		l = strings.TrimSpace(l)
		if l == "" || l[0] != '{' {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Part      struct {
				Text string `json:"text"`
			} `json:"part"`
		}
		if json.Unmarshal([]byte(l), &ev) != nil {
			continue
		}
		if ev.SessionID != "" {
			session = ev.SessionID
		}
		if ev.Type == "text" && strings.TrimSpace(ev.Part.Text) != "" {
			text = append(text, ev.Part.Text)
		}
	}
	return strings.TrimSpace(strings.Join(text, "\n")), session
}

// "provider/model" per line (opencode models)
func parseOpenCodeModels(out string) []string {
	var ids []string
	for _, l := range lines(out) {
		if l = strings.TrimSpace(l); strings.Contains(l, "/") && !strings.Contains(l, " ") {
			ids = append(ids, l)
		}
	}
	return ids
}
