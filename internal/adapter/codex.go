package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Codex drives the OpenAI Codex CLI: `codex exec` for runs and the official
// `codex app-server` JSON-RPC protocol for quota and model discovery.
type Codex struct{}

func (Codex) Name() string   { return "codex" }
func (Codex) Binary() string { return "codex" }
func (Codex) Capabilities() Capabilities {
	return Capabilities{SessionResume: true, StructuredOutput: true, ReadOnly: true, FileEditing: true}
}

type rpcMsg struct {
	ID     *int            `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// appServerCall performs initialize + one request against a short-lived
// `codex app-server` over stdio and returns the raw result.
func appServerCall(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "-s", "read-only", "-a", "never", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() }()

	enc := json.NewEncoder(stdin)
	enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"clientInfo": map[string]string{"name": "dispatch", "version": "0.1.0"}}})
	enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "initialized"})
	req := map[string]any{"jsonrpc": "2.0", "id": 2, "method": method}
	if params != nil {
		req["params"] = params
	}
	enc.Encode(req)

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	for sc.Scan() {
		var m rpcMsg
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.ID == nil || *m.ID != 2 {
			continue
		}
		if m.Error != nil {
			return nil, fmt.Errorf("codex app-server: %s", m.Error.Message)
		}
		return m.Result, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("codex app-server closed without a response")
}

type codexWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int    `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

// Quota prefers the secondary (weekly) window and falls back to primary.
func (Codex) Quota(ctx context.Context) (Quota, error) {
	raw, err := appServerCall(ctx, "account/rateLimits/read", nil)
	if err != nil {
		return Quota{}, fmt.Errorf("%w: %v", ErrNoQuota, err)
	}
	var res struct {
		RateLimits struct {
			Primary   *codexWindow `json:"primary"`
			Secondary *codexWindow `json:"secondary"`
		} `json:"rateLimits"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return Quota{}, fmt.Errorf("%w: %v", ErrNoQuota, err)
	}
	w := res.RateLimits.Secondary
	if w == nil {
		w = res.RateLimits.Primary
	}
	if w == nil || w.WindowDurationMins == nil || w.ResetsAt == nil {
		return Quota{}, ErrNoQuota
	}
	return Quota{
		Key:           "codex",
		UsedPercent:   w.UsedPercent,
		WindowMinutes: *w.WindowDurationMins,
		ResetsAt:      time.Unix(*w.ResetsAt, 0),
	}, nil
}

func (Codex) Models(ctx context.Context) ([]Model, error) {
	raw, err := appServerCall(ctx, "model/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res struct {
		Data []struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	var out []Model
	for _, m := range res.Data {
		id := m.Model
		if id == "" {
			id = m.ID
		}
		// Tier assignment lives in the declarative tier table, not here.
		out = append(out, Model{ID: id})
	}
	return out, nil
}

// Run executes `codex exec` (or `codex exec resume`), returning the agent's
// last message. The session id comes from the --json thread.started event.
func (Codex) Run(ctx context.Context, req Request, model string) (Result, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	out, err := os.CreateTemp("", "dispatch-codex-*.txt")
	if err != nil {
		return Result{}, err
	}
	out.Close()
	defer os.Remove(out.Name())

	sandbox := "workspace-write"
	if req.ReadOnly {
		sandbox = "read-only"
	}
	// `exec resume` has no -s/-C flags, so sandbox goes through config and
	// the working directory through the process.
	args := []string{"exec"}
	if req.ResumeSession != "" {
		args = append(args, "resume")
	}
	args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", sandbox),
		"--json", "--skip-git-repo-check", "-o", out.Name())
	if model != "" {
		args = append(args, "-m", model)
	}
	if req.ResumeSession != "" {
		args = append(args, req.ResumeSession)
	}
	args = append(args, "-") // prompt from stdin
	cmd := exec.CommandContext(ctx, "codex", args...)
	ownGroup(cmd)
	cmd.Dir = req.Dir
	cmd.Stdin = bytes.NewBufferString(req.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	res := Result{ExitCode: cmd.ProcessState.ExitCode(), SessionID: req.ResumeSession}
	if id := codexThreadID(stdout.Bytes()); id != "" {
		res.SessionID = id
	}
	if b, err := os.ReadFile(out.Name()); err == nil {
		res.Output = string(b)
	}
	if runErr != nil {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, fmt.Errorf("codex exec: %v: %s", runErr, bytes.TrimSpace(stderr.Bytes()))
	}
	return res, nil
}

// codexThreadID finds the thread id in `codex exec --json` JSONL output.
func codexThreadID(stdout []byte) string {
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		var ev struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Type == "thread.started" {
			return ev.ThreadID
		}
	}
	return ""
}
