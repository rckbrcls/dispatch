package adapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
)

// CreditRe matches provider errors meaning the account cannot run this model
// until the user acts (buy credits, upgrade the plan). Waiting a few minutes
// does not help, unlike a rate limit, so the model is avoided for longer.
var CreditRe = regexp.MustCompile(`(?i)ActionRequiredError|not included in your (?:\w+ )?(?:usage|plan|subscription)|buy (?:more )?credits|purchase (?:more )?credits|payment required|\b402\b|upgrade (?:your )?(?:plan|subscription) to (?:use|continue|access)`)

// errFailFast is the cancel cause set when stderr prints a CreditRe line.
var errFailFast = errors.New("fatal agent output")

// failFast derives a context that is cancelled as soon as the agent's stderr
// prints a line matching CreditRe, so the run does not wait for a CLI that
// keeps going (or hangs) after a fatal account error. Only stderr is watched:
// stdout carries the agent's own work, which may mention these words.
// fatal reports the matched line, or "" when the run ended on its own.
func failFast(ctx context.Context) (fctx context.Context, stderr io.Writer, fatal func() string, cancel context.CancelFunc) {
	fctx, cancelCause := context.WithCancelCause(ctx)
	w := &fatalWatch{cancel: cancelCause}
	fatal = func() string {
		if !errors.Is(context.Cause(fctx), errFailFast) {
			return ""
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.hit
	}
	return fctx, w, fatal, func() { cancelCause(nil) }
}

type fatalWatch struct {
	mu     sync.Mutex
	cancel context.CancelCauseFunc
	line   []byte
	hit    string
}

func (w *fatalWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.hit != "" {
		return len(p), nil
	}
	w.line = append(w.line, p...)
	for {
		i := bytes.IndexByte(w.line, '\n')
		if i < 0 {
			break
		}
		if w.check(w.line[:i]) {
			return len(p), nil
		}
		w.line = w.line[i+1:]
	}
	// CLIs often print the error without a trailing newline and then stall.
	if len(w.line) > 0 && w.check(w.line) {
		return len(p), nil
	}
	if len(w.line) > 64<<10 {
		w.line = nil
	}
	return len(p), nil
}

func (w *fatalWatch) check(line []byte) bool {
	s := strings.TrimSpace(string(line))
	if !CreditRe.MatchString(s) {
		return false
	}
	w.hit, w.line = s, nil
	w.cancel(errFailFast)
	return true
}
