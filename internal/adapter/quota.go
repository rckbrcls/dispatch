package adapter

import (
	"encoding/json"
	"time"
)

const (
	SourceCodexAppServer = "codex app-server"
	SourceAgyUsage       = "agy /usage"
	SourceEmpirical      = "empirical"
)

// Confidence is how a quota figure was obtained.
type Confidence string

const (
	ConfidenceOfficial  Confidence = "official"
	ConfidenceEmpirical Confidence = "empirical"
)

// Span is a time.Duration that JSON-encodes as a Go duration string.
type Span time.Duration

func (s Span) Duration() time.Duration { return time.Duration(s) }

func (s Span) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(s).String())
}

func (s *Span) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var text string
	if err := json.Unmarshal(b, &text); err != nil {
		return err
	}
	d, err := time.ParseDuration(text)
	if err != nil {
		return err
	}
	*s = Span(d)
	return nil
}

// QuotaSample is one usage window.
// Used is nil when the adapter has no official percentage. Callers must not
// treat that absence as zero.
type QuotaSample struct {
	Window     string     `json:"window"`
	Used       *float64   `json:"used,omitempty"`
	ResetAt    time.Time  `json:"resetAt,omitzero"`
	Duration   Span       `json:"duration,omitempty"`
	Source     string     `json:"source"`
	Confidence Confidence `json:"confidence"`
	// Risk is set for empirical samples only (cooldown, recent rate limit).
	Risk string `json:"risk,omitempty"`
}

// EmpiricalSample reports risk from a cooldown and recent rate-limit hits.
// It never fills Used.
func EmpiricalSample(now, cooledUntil time.Time, recentLimits int) QuotaSample {
	s := QuotaSample{
		Window:     "empirical",
		Source:     SourceEmpirical,
		Confidence: ConfidenceEmpirical,
		Risk:       "unknown",
	}
	switch {
	case !cooledUntil.IsZero() && cooledUntil.After(now):
		s.Risk = "cooldown"
		s.ResetAt = cooledUntil
	case recentLimits > 0:
		s.Risk = "recent_rate_limit"
	}
	return s
}
