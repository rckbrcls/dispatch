package adapter

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCreditRe(t *testing.T) {
	for _, s := range []string{
		"ActionRequiredError: GPT-5.6 Sol is not included in your weekly usage Buy credits or switch to a Grok model to continue.",
		"This model is not included in your plan",
		"HTTP 402 Payment Required",
		"Please buy credits to continue",
	} {
		if !CreditRe.MatchString(s) {
			t.Errorf("should match %q", s)
		}
	}
	for _, s := range []string{"429 too many requests", "credit card form validation", "port 14020 busy", "added credits to the ledger table"} {
		if CreditRe.MatchString(s) {
			t.Errorf("should not match %q", s)
		}
	}
}

func TestFailFastWatch(t *testing.T) {
	ctx, w, fatal, stop := failFast(context.Background())
	defer stop()
	w.Write([]byte("warming up\n"))
	if ctx.Err() != nil || fatal() != "" {
		t.Fatal("ordinary line cancelled the run")
	}
	// No trailing newline: CLIs often stall right after printing it.
	w.Write([]byte("ActionRequiredError: not included in your weekly usage"))
	if ctx.Err() == nil || !strings.HasPrefix(fatal(), "ActionRequiredError") {
		t.Fatalf("not cancelled: %q", fatal())
	}
}

func TestFailFastStopsHangingAgent(t *testing.T) {
	ctx, w, fatal, stop := failFast(context.Background())
	defer stop()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `echo "ActionRequiredError: buy credits" >&2; sleep 30`)
	cmd.Stderr = w
	start := time.Now()
	runAgent(cmd, nil)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
	if fatal() == "" {
		t.Fatal("fatal line not reported")
	}
}

func TestFailFastIgnoresNormalExit(t *testing.T) {
	ctx, _, fatal, stop := failFast(context.Background())
	stop()
	if ctx.Err() == nil || fatal() != "" {
		t.Fatal("plain cancel must not look fatal")
	}
}
