// Package adapter defines the contract between dispatch and each agent CLI.
package adapter

import (
	"context"
	"errors"
	"time"
)

// Tier is the difficulty level a caller requests instead of a model name.
type Tier string

const (
	TierEasy   Tier = "easy"
	TierNormal Tier = "normal"
	TierHard   Tier = "hard"
)

func (t Tier) Valid() bool {
	return t == TierEasy || t == TierNormal || t == TierHard
}

// Capabilities are what an adapter claims. Probe checks the flags behind
// them. A run that needs a capability the CLI does not provide is refused.
// Usage is true only when an official quota source exists.
type Capabilities struct {
	EffortMode       string   `json:"effortMode"` // config, model-suffix, or unsupported (empty)
	Efforts          []string `json:"efforts,omitempty"`
	SessionResume    bool     `json:"sessionResume"`
	StructuredOutput bool     `json:"structuredOutput"`
	ReadOnly         bool     `json:"readOnly"`
	FileEditing      bool     `json:"fileEditing"`
	Usage            bool     `json:"usage"`
}

// Quota is a normalized usage window reported by a collector.
type Quota struct {
	// Key identifies the quota pool (e.g. "codex", "antigravity:Gemini Models").
	Key           string        `json:"key,omitempty"`
	UsedPercent   float64       `json:"usedPercent"`
	WindowMinutes int           `json:"windowMinutes"`
	ResetsAt      time.Time     `json:"resetsAt"`
	Window        time.Duration `json:"-"`
}

// Request is a single delegated task.
type Request struct {
	Effort   string
	Prompt   string
	Tier     Tier
	Dir      string
	ReadOnly bool
	Timeout  time.Duration
	// ResumeSession continues a previous session of the same adapter.
	ResumeSession string
	// Env passes a sanitized environment to the child agent subprocess.
	Env []string
	// Started persists the agent group leader before waiting for it.
	Started func(int) error
}

// Result is the raw outcome of a run, before compaction.
type Result struct {
	Output   string
	ExitCode int
	// SessionID identifies the agent session so it can be resumed later.
	SessionID string
}

// ModelQuoter is implemented by adapters whose quota differs per model
// (e.g. Antigravity's Gemini vs third-party groups). It returns a quota for
// each requested model it can resolve.
type ModelQuoter interface {
	ModelQuotas(ctx context.Context, models []string) (map[string]Quota, error)
}

// Adapter wraps one agent CLI.
type Adapter interface {
	// Name is the stable identifier (e.g. "codex").
	Name() string
	// Binary is the executable looked up on PATH.
	Binary() string
	Capabilities() Capabilities
	// Models lists the models this CLI can run, with the tier each serves.
	// Discovery is best-effort; adapters may return a static list.
	Models(ctx context.Context) ([]Model, error)
	// Quota reports current usage; return ErrNoQuota when unavailable.
	Quota(ctx context.Context) (Quota, error)
	Run(ctx context.Context, req Request, model string) (Result, error)
}

// Prober inspects one CLI without invoking a model.
// Probe runs only `<binary> --version` and `<binary> --help`.
type Prober interface {
	Probe(ctx context.Context) ProbeReport
}

// UsageProvider is implemented by adapters with an official quota source.
// Adapters without one must not invent a used percentage.
type UsageProvider interface {
	UsageSource() string
	Usage(ctx context.Context) ([]QuotaSample, error)
}

// Resumer continues a previous session. Adapters that cannot resume omit it.
type Resumer interface {
	Resume(ctx context.Context, req Request, model string) (Result, error)
}

// Model is a model offered by an adapter and the tier it is declared for.
type Model struct {
	ID   string `json:"id"`
	Tier Tier   `json:"tier"`
}

// ErrNoQuota is returned by adapters with no reliable usage source.
var ErrNoQuota = errors.New("quota unavailable")

// ErrNotImplemented marks adapters whose non-interactive flags are not yet
// confirmed against the CLI's official documentation.
var ErrNotImplemented = errors.New("run not implemented")
