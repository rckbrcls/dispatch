package runner

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
)

type fake struct {
	name string
	out  string
	err  error
	ran  *[]string
}

func (f fake) Name() string                                    { return f.name }
func (f fake) Binary() string                                  { return f.name }
func (f fake) Capabilities() adapter.Capabilities              { return adapter.Capabilities{ReadOnly: true} }
func (f fake) Models(context.Context) ([]adapter.Model, error) { return nil, nil }
func (f fake) Quota(context.Context) (adapter.Quota, error) {
	return adapter.Quota{}, adapter.ErrNoQuota
}
func (f fake) Run(_ context.Context, r adapter.Request, m string) (adapter.Result, error) {
	tag := f.name + "/" + m
	if r.ResumeSession != "" {
		tag += "@" + r.ResumeSession
	}
	*f.ran = append(*f.ran, tag)
	return adapter.Result{Output: f.out, SessionID: "sess-" + f.name}, f.err
}

func deps(t *testing.T, ran *[]string, a ...adapter.Adapter) Deps {
	t.Setenv(depthEnv, "")
	t.Setenv(noDelegateEnv, "")
	return Deps{
		Adapters: a,
		Table: tiers.Table{Tiers: map[adapter.Tier][]tiers.Entry{
			adapter.TierEasy: {{Agent: "a", Model: "m1"}, {Agent: "b", Model: "m2"}, {Agent: "ghost", Model: "x"}},
		}},
		Store:       store.Store{Path: filepath.Join(t.TempDir(), "runs.jsonl")},
		Now:         time.Now,
		Installed:   func(b string) bool { return b != "ghost" },
		WorktreeDir: filepath.Join(t.TempDir(), "wt"),
	}
}

func TestFallback(t *testing.T) {
	var ran []string
	d := deps(t, &ran,
		fake{name: "a", err: errors.New("rate limited"), ran: &ran},
		fake{name: "b", out: "hello\n", ran: &ran})
	rec, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "p"})
	if err != nil || rec.Status != store.Completed || rec.Agent != "b" || len(rec.Attempts) != 1 {
		t.Fatalf("%+v %v", rec, err)
	}
	if got, _ := d.Store.Get(rec.ID); got.Status != store.Completed {
		t.Fatalf("not persisted: %+v", got)
	}
}

func TestAllFail(t *testing.T) {
	var ran []string
	d := deps(t, &ran, fake{name: "a", err: errors.New("x"), ran: &ran}, fake{name: "b", err: errors.New("y"), ran: &ran})
	rec, _ := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "p"})
	if rec.Status != store.Failed || len(ran) != 2 {
		t.Fatalf("%+v ran=%v", rec, ran)
	}
}

func TestTimeoutStopsFallback(t *testing.T) {
	var ran []string
	d := deps(t, &ran, fake{name: "a", err: context.DeadlineExceeded, ran: &ran}, fake{name: "b", out: "x", ran: &ran})
	rec, _ := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "p", Timeout: time.Second})
	if rec.Status != store.TimedOut || len(ran) != 1 {
		t.Fatalf("%+v ran=%v", rec, ran)
	}
}

func TestRecursionRefused(t *testing.T) {
	var ran []string
	d := deps(t, &ran)
	t.Setenv(depthEnv, "1")
	if _, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy}); !errors.Is(err, ErrRecursive) {
		t.Fatalf("got %v", err)
	}
}

func TestNoDelegateRefused(t *testing.T) {
	// (c) run com DISPATCH_NO_DELEGATE recusa
	var ran []string
	d := deps(t, &ran)
	t.Setenv(noDelegateEnv, "1")
	if _, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy}); !errors.Is(err, ErrRecursive) {
		t.Fatalf("got %v, want ErrRecursive", err)
	}
}

type fakeEnvAdapter struct {
	fake
	capturedEnv *[]string
}

func (f fakeEnvAdapter) Run(ctx context.Context, r adapter.Request, m string) (adapter.Result, error) {
	*f.capturedEnv = append([]string(nil), r.Env...)
	return f.fake.Run(ctx, r, m)
}

func TestChildEnvironmentVariables(t *testing.T) {
	var ran []string
	var env []string
	f := fakeEnvAdapter{
		fake:        fake{name: "a", out: "ok", ran: &ran},
		capturedEnv: &env,
	}
	d := deps(t, &ran, f)
	t.Setenv(runIDEnv, "parent-run-456")

	rec, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}

	mustContain := map[string]string{
		"DISPATCH_DEPTH":         "1",
		"DISPATCH_RUN_ID":        rec.ID,
		"DISPATCH_PARENT_RUN_ID": "parent-run-456",
		"DISPATCH_NO_DELEGATE":   "1",
	}

	envMap := map[string]string{}
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		envMap[k] = v
	}

	for k, want := range mustContain {
		if got := envMap[k]; got != want {
			t.Errorf("env[%s] = %q, want %q", k, got, want)
		}
	}
}

func TestPageAndSummary(t *testing.T) {
	text := "a\nb\nc\nd\n"
	c, total, more := Page(text, 1, 2)
	if c != "b\nc\n" || total != 4 || !more {
		t.Fatalf("%q %d %v", c, total, more)
	}
	if _, _, more := Page(text, 2, 10); more {
		t.Fatal("no more expected")
	}
	s, trunc := Summary("l1\nl2\nl3\n", 2, 100)
	if s != "l1\nl2\n" || !trunc {
		t.Fatalf("%q %v", s, trunc)
	}
	if s, trunc := Summary("short", 10, 100); s != "short" || trunc {
		t.Fatalf("%q %v", s, trunc)
	}
}

func TestResumeUsesSameAgentAndSession(t *testing.T) {
	var ran []string
	d := deps(t, &ran, fake{name: "a", out: "one", ran: &ran}, fake{name: "b", out: "two", ran: &ran})
	first, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "p", ReadOnly: true})
	if err != nil || first.SessionID != "sess-a" {
		t.Fatalf("%+v %v", first, err)
	}
	ran = nil
	second, err := d.Run(context.Background(), Options{Prompt: "follow up", ReadOnly: true, Resume: first.ID})
	if err != nil || second.Status != store.Completed || second.ResumedOf != first.ID {
		t.Fatalf("%+v %v", second, err)
	}
	if len(ran) != 1 || ran[0] != "a/m1@sess-a" {
		t.Fatalf("ran = %v", ran)
	}
}

func TestRateLimitSetsCooldownAndDemotes(t *testing.T) {
	var ran []string
	d := deps(t, &ran,
		fake{name: "a", err: errors.New("429 too many requests"), ran: &ran},
		fake{name: "b", out: "ok", ran: &ran})
	d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "p", ReadOnly: true})
	if _, ok := d.Store.Cooldowns(time.Now())["a"]; !ok {
		t.Fatal("expected cooldown for a")
	}
	ran = nil
	d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "p", ReadOnly: true})
	if len(ran) == 0 || ran[0] != "b/m2" {
		t.Fatalf("cooled-down agent should be tried last, ran = %v", ran)
	}
}

func TestCancelOrphanAndWait(t *testing.T) {
	var ran []string
	d := deps(t, &ran)
	rec := store.Record{ID: "r-1", Status: store.Running, PID: 999999, CreatedAt: time.Now()}
	d.Store.Append(rec)
	out, err := d.Cancel(context.Background(), "r-1")
	if err != nil || out.Status != store.Cancelled {
		t.Fatalf("%+v %v", out, err)
	}
	rec2 := store.Record{ID: "r-2", Status: store.Running, PID: 999999, CreatedAt: time.Now()}
	d.Store.Append(rec2)
	w, err := d.Wait(context.Background(), "r-2", 10*time.Millisecond)
	if err != nil || w.Status != store.Failed {
		t.Fatalf("%+v %v", w, err)
	}
}
