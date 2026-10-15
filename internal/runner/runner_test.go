package runner

import (
	"context"
	"errors"
	"path/filepath"
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
func (f fake) Run(_ context.Context, _ adapter.Request, m string) (adapter.Result, error) {
	*f.ran = append(*f.ran, f.name+"/"+m)
	return adapter.Result{Output: f.out}, f.err
}

func deps(t *testing.T, ran *[]string, a ...adapter.Adapter) Deps {
	t.Setenv(depthEnv, "")
	return Deps{
		Adapters: a,
		Table: tiers.Table{Tiers: map[adapter.Tier][]tiers.Entry{
			adapter.TierEasy: {{Agent: "a", Model: "m1"}, {Agent: "b", Model: "m2"}, {Agent: "ghost", Model: "x"}},
		}},
		Store:     store.Store{Path: filepath.Join(t.TempDir(), "runs.jsonl")},
		Now:       time.Now,
		Installed: func(b string) bool { return b != "ghost" },
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
