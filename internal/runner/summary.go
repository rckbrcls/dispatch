package runner

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"dispatch/internal/store"
	"dispatch/internal/verify"
)

const SummarySchema = "dispatch.run-summary.v1"

// effortSuffixes are the model-name tails that name an effort level.
var effortSuffixes = map[string]bool{
	"none": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true,
}

var (
	conflictRe = regexp.MustCompile(`(?i)merge conflict|conflict marker|\bCONFLICT\b|<<<<<<<`)
	crashRe    = regexp.MustCompile(`(?i)\bpanic\b|segmentation fault|core dumped|signal:|exit status 13[479]|\baborted\b`)
)

// EffortOf returns the effort encoded as the last hyphen-suffix of a model
// name (gemini-3.8-flash-high → high). An empty result means the name does
// not carry one; callers leave the field absent.
func EffortOf(model string) string {
	i := strings.LastIndex(model, "-")
	if i < 0 || i+1 >= len(model) {
		return ""
	}
	suf := strings.ToLower(model[i+1:])
	if effortSuffixes[suf] {
		return suf
	}
	return ""
}

// ProviderOf derives a provider from the model name, then from the agent.
// An empty result means unknown.
func ProviderOf(agent, model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gemini"):
		return "google"
	case strings.Contains(m, "claude"):
		return "anthropic"
	case strings.Contains(m, "grok"):
		return "xai"
	case strings.Contains(m, "composer"):
		return "cursor"
	case strings.Contains(m, "gpt-") || strings.HasPrefix(m, "gpt") || strings.Contains(m, "codex") ||
		strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4"):
		return "openai"
	}
	switch strings.ToLower(agent) {
	case "codex":
		return "openai"
	case "grok":
		return "xai"
	case "cursor":
		return "cursor"
	case "antigravity":
		return "google"
	default:
		return ""
	}
}

// classOf labels an agent error. "failed" is the generic bucket.
func classOf(msg string) string {
	switch {
	case rateLimited.MatchString(msg):
		return "rate_limit"
	case conflictRe.MatchString(msg):
		return "merge_conflict"
	case crashRe.MatchString(msg):
		return "crash"
	default:
		return "failed"
	}
}

// Classify refines ExitClass with rate_limit, crash and merge_conflict.
func Classify(rec store.Record) string {
	if rec.Status == store.Failed {
		if n := len(rec.Attempts); n > 0 {
			switch rec.Attempts[n-1].ExitClass {
			case "rate_limit", "crash", "merge_conflict":
				return rec.Attempts[n-1].ExitClass
			}
		}
		if cls := classOf(rec.Error); cls != "failed" {
			return cls
		}
	}
	if rec.Status == store.Completed && conflictOnly(rec.Verify) {
		return "merge_conflict"
	}
	if rec.Status == store.Completed && rec.Verification != string(verify.StatusFailed) &&
		(conflictRe.MatchString(rec.Note) || conflictRe.MatchString(rec.Error)) {
		return "merge_conflict"
	}
	return ExitClass(rec.Status, rec.Verification)
}

func conflictOnly(res *verify.Result) bool {
	if res == nil {
		return false
	}
	saw := false
	for _, c := range res.Checks {
		if c.Status != verify.CheckFailed && c.Status != verify.CheckTimeout {
			continue
		}
		if c.Name != "conflicts" {
			return false
		}
		saw = true
	}
	return saw
}

// tries is how many agents were invoked. Failed attempts are recorded; the
// final agent (success, timeout, cancel, orphan, or the one still running)
// is not in that slice.
func tries(rec store.Record) int {
	n := len(rec.Attempts)
	switch rec.Status {
	case store.Completed, store.TimedOut, store.Cancelled, store.Orphaned, store.Running:
		if rec.Agent != "" {
			n++
		}
	}
	return n
}

// RunSummary is the dispatch.run-summary.v1 document. It never carries the
// agent's free text; that stays behind `dispatch result <id> --section agent`.
type RunSummary struct {
	Schema       string            `json:"schema"`
	ID           string            `json:"id"`
	Status       store.Status      `json:"status"`
	Agent        string            `json:"agent,omitempty"`
	Model        string            `json:"model,omitempty"`
	Tier         string            `json:"tier,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	Effort       string            `json:"effort,omitempty"`
	DurationMs   *int64            `json:"durationMs,omitempty"`
	QueuedMs     *int64            `json:"queuedMs,omitempty"`
	RunMs        *int64            `json:"runMs,omitempty"`
	VerifyMs     *int64            `json:"verifyMs,omitempty"`
	FilesChanged *int              `json:"filesChanged,omitempty"`
	Insertions   *int              `json:"insertions,omitempty"`
	Deletions    *int              `json:"deletions,omitempty"`
	Verification string            `json:"verification,omitempty"`
	Attempts     int               `json:"attempts"`
	ExitClass    string            `json:"exitClass,omitempty"`
	Branch       string            `json:"branch,omitempty"`
	Worktree     string            `json:"worktree,omitempty"`
	Error        string            `json:"error,omitempty"`
	Note         string            `json:"note,omitempty"`
	SessionID    string            `json:"sessionId,omitempty"`
	ResumedOf    string            `json:"resumedOf,omitempty"`
	Details      map[string]string `json:"details"`
}

// Summarize builds a RunSummary. Absent numbers stay absent.
func Summarize(rec store.Record) RunSummary {
	s := RunSummary{
		Schema:       SummarySchema,
		ID:           rec.ID,
		Status:       rec.Status,
		Agent:        rec.Agent,
		Model:        rec.Model,
		Tier:         rec.Tier,
		Provider:     rec.Provider,
		Effort:       rec.Effort,
		QueuedMs:     rec.QueuedMs,
		RunMs:        rec.RunMs,
		FilesChanged: rec.FilesChanged,
		Insertions:   rec.Insertions,
		Deletions:    rec.Deletions,
		Verification: rec.Verification,
		Attempts:     tries(rec),
		ExitClass:    rec.ExitClass,
		Branch:       rec.Branch,
		Worktree:     rec.Worktree,
		Error:        rec.Error,
		Note:         rec.Note,
		SessionID:    rec.SessionID,
		ResumedOf:    rec.ResumedOf,
		Details:      detailCmds(rec.ID),
	}
	if ms, ok := rec.ObservedMs(); ok {
		s.DurationMs = store.I64(ms)
	}
	if rec.Verification != "" || rec.Verify != nil {
		v := rec.VerifyMs
		s.VerifyMs = &v
	}
	return s
}

func detailCmds(id string) map[string]string {
	sec := func(name string) string {
		return "dispatch result " + id + " --section " + name
	}
	return map[string]string{
		"summary":      sec("summary"),
		"verification": sec("verification"),
		"files":        sec("files"),
		"agent":        sec("agent"),
		"attempts":     sec("attempts"),
		"meta":         sec("meta"),
	}
}

// Text is the three-line human rendering. Unknown fields say "desconhecido".
func (s RunSummary) Text() string {
	line1 := fmt.Sprintf("%s %s/%s tier=%s %s %s verification=%s attempts=%d",
		s.Status, unk(s.Agent), unk(s.Model), unk(s.Tier), fmtMs(s.DurationMs), fmtFiles(s), unk(s.Verification), s.Attempts)
	line2 := fmt.Sprintf("branch=%s worktree=%s", unk(s.Branch), unk(s.Worktree))
	line3 := fmt.Sprintf("details: dispatch result %s --section summary|verification|files|agent|attempts|meta", s.ID)
	return line1 + "\n" + line2 + "\n" + line3 + "\n"
}

func unk(s string) string {
	if s == "" {
		return "desconhecido"
	}
	return s
}

func fmtMs(ms *int64) string {
	if ms == nil {
		return "desconhecido"
	}
	if *ms < 1000 {
		return fmt.Sprintf("%dms", *ms)
	}
	return (time.Duration(*ms) * time.Millisecond).Round(100 * time.Millisecond).String()
}

func fmtFiles(s RunSummary) string {
	if s.FilesChanged == nil {
		return "files=desconhecido"
	}
	if s.Insertions == nil || s.Deletions == nil {
		return fmt.Sprintf("files=%d +/- desconhecido", *s.FilesChanged)
	}
	return fmt.Sprintf("files=%d +%d -%d", *s.FilesChanged, *s.Insertions, *s.Deletions)
}
