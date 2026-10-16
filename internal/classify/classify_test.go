package classify

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"dispatch/internal/adapter"
	"dispatch/internal/store"
	"dispatch/internal/verify"
)

func TestPriorSensitiveRules(t *testing.T) {
	for _, prompt := range []string{
		"corrigir auth em internal/auth.go", "refactor permissions", "corrigir permissões",
		"criar pagamento em dinheiro", "fix database", "migração de dados", "security patch",
	} {
		for _, tier := range []adapter.Tier{adapter.TierEasy, adapter.TierNormal, adapter.TierHard} {
			f := Extract(Input{Prompt: prompt, Tier: tier, Dir: t.TempDir()})
			p := Prior().Predict(tier, f, false)
			if !f.Sensitive() || p.Score < Threshold || p.SuggestedTier != Higher(tier) {
				t.Errorf("%q %s: %+v", prompt, tier, p)
			}
			if p.Reasons[0].Contribution <= 0 || !strings.HasPrefix(p.Reasons[0].Feature, "risk_") {
				t.Fatalf("missing risk explanation: %+v", p)
			}
		}
	}
	for _, prompt := range []string{"corrigir typo", "criar README", "investigar testes; go test ./...", "refatorar helper"} {
		f := Extract(Input{Prompt: prompt, Tier: adapter.TierEasy, Dir: t.TempDir()})
		if p := Prior().Predict(adapter.TierEasy, f, false); f.Sensitive() || p.SuggestedTier != adapter.TierEasy {
			t.Fatalf("neutral prompt %q: %+v", prompt, p)
		}
	}
}

func TestFeaturesDeterministicAndBounded(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "internal", "foo"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "foo", "foo.go"), []byte("never read"), 0000); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=never-read"), 0000); err != nil {
		t.Fatal(err)
	}
	in := Input{Prompt: "1. Criar e refatorar internal/foo/foo.go internal/foo internal/foo/foo.go\n- corrigir investigar testar\n```go test ./...``` .env linked ../escape /etc/passwd", Dir: dir, ReadOnly: true, Tier: adapter.TierNormal}
	f := Extract(in)
	for range 10 {
		if got := Extract(in); got != f {
			t.Fatalf("not deterministic: %v != %v", got, f)
		}
	}
	if f[6] != 1.0/20 || f[7] != 1.0/10 || f[18] != 1 || f[19] != 1 || f[20] != 0 || f[22] != 1 || f[4] != 2.0/20 {
		t.Fatalf("features %v", f)
	}
	for i := 8; i <= 12; i++ {
		if f[i] != 1 {
			t.Errorf("verb %s missing", names[i])
		}
	}
	big := Extract(Input{Prompt: strings.Repeat("- test\n```\n", 2000), Dir: dir})
	if big[1] != 1 || big[3] != 1 || big[4] != 1 || big[5] != 1 {
		t.Fatalf("saturation: %v", big)
	}
	for i, x := range big {
		if x < 0 || x > 1 || math.IsNaN(x) {
			t.Errorf("feature %d: %f", i, x)
		}
	}
}

func TestTrainingConvergesPerTier(t *testing.T) {
	// Deliberately invert the hand prior: creates fail, auth fixes succeed.
	dir := t.TempDir()
	xs := []Features{
		Extract(Input{Prompt: "create helper", Tier: adapter.TierEasy, Dir: dir}),
		Extract(Input{Prompt: "fix auth", Tier: adapter.TierEasy, Dir: dir}),
	}
	ys := []bool{true, false}
	m := Prior()
	metrics := func() (loss, accuracy float64) {
		for i, x := range xs {
			p := m.Predict(adapter.TierEasy, x, false).Score
			y := 0.0
			if ys[i] {
				y = 1
			}
			loss -= y*math.Log(p) + (1-y)*math.Log(1-p)
			if (p >= 0.5) == ys[i] {
				accuracy++
			}
		}
		return loss / 2, accuracy / 2
	}
	beforeLoss, beforeAcc := metrics()
	for i := range MinSamples - 1 {
		m.Learn(adapter.TierEasy, xs[i%2], ys[i%2])
	}
	if m.Tiers[adapter.TierEasy].Weights != Prior().Tiers[adapter.TierEasy].Weights {
		t.Fatal("updated before warmup")
	}
	for range 2000 {
		for i, x := range xs {
			m.Learn(adapter.TierEasy, x, ys[i])
		}
	}
	afterLoss, afterAcc := metrics()
	if afterLoss >= beforeLoss/4 || afterAcc <= beforeAcc || afterAcc != 1 {
		t.Fatalf("loss %.4f -> %.4f accuracy %.2f -> %.2f", beforeLoss, afterLoss, beforeAcc, afterAcc)
	}
	for _, tier := range []adapter.Tier{adapter.TierNormal, adapter.TierHard} {
		if m.Tiers[tier] != Prior().Tiers[tier] {
			t.Fatal("training leaked across tiers")
		}
	}
}

func TestTrainingNumericallySafe(t *testing.T) {
	m := Prior()
	f := Features{}
	for i := range f {
		f[i] = math.Inf(1)
	}
	f[2], f[3], f[4] = math.NaN(), -math.MaxFloat64, math.MaxFloat64
	for i := range 10000 {
		m.Learn(adapter.TierHard, f, i%2 == 0)
		p := m.Predict(adapter.TierHard, f, true)
		if math.IsNaN(p.Score) || math.IsInf(p.Score, 0) || p.Score <= 0 || p.Score >= 1 || !m.valid() {
			t.Fatalf("unstable: %+v", p)
		}
	}
	h := m.Tiers[adapter.TierHard]
	h.Weights[0] = math.NaN()
	m.Tiers[adapter.TierHard] = h
	m.Learn(adapter.TierHard, f, true)
	if !m.valid() {
		t.Fatal("invalid weights were not recovered")
	}
}

func TestLabels(t *testing.T) {
	base := store.Record{Status: store.Completed, Verification: "verified", ExitClass: "ok"}
	if failed, ok := Label(base); failed || !ok {
		t.Fatal("verified label")
	}
	base.Verification, base.ExitClass = "verification_failed", "verify_fail"
	if failed, ok := Label(base); !failed || !ok {
		t.Fatal("failure label")
	}
	for _, cls := range []string{"rate_limit", "crash", "timeout", "cancelled", "merge_conflict", "failed"} {
		r := base
		r.ExitClass = cls
		if _, ok := Label(r); ok {
			t.Errorf("accepted %s", cls)
		}
	}
	for _, status := range []store.Status{store.Pending, store.Running, store.Failed, store.Cancelled, store.TimedOut, store.Orphaned} {
		r := base
		r.Status = status
		if _, ok := Label(r); ok {
			t.Errorf("accepted %s", status)
		}
	}
	for _, verdict := range []string{"", "unverified", "error"} {
		r := base
		r.Verification = verdict
		if _, ok := Label(r); ok {
			t.Errorf("accepted verdict %q", verdict)
		}
	}
	for _, mutate := range []func(*store.Record){
		func(r *store.Record) { r.NoVerify = true },
		func(r *store.Record) { r.ReadOnly = true },
		func(r *store.Record) { r.Verify = &verify.Result{Status: verify.StatusError} },
		func(r *store.Record) {
			r.Verify = &verify.Result{Status: verify.StatusFailed, Checks: []verify.CheckResult{{Status: verify.CheckTimeout}}}
		},
		func(r *store.Record) {
			r.Verify = &verify.Result{Status: verify.StatusFailed, Checks: []verify.CheckResult{{Name: "conflicts", Regressions: []string{"x"}}}}
		},
	} {
		r := base
		mutate(&r)
		if _, ok := Label(r); ok {
			t.Errorf("accepted %+v", r)
		}
	}
	base.Attempts = []store.Attempt{{ExitClass: "rate_limit"}}
	if _, ok := Label(base); !ok {
		t.Fatal("a recovered fallback should retain the final verifier label")
	}
}

func observedRecord(t *testing.T) store.Record {
	t.Helper()
	f := Extract(Input{Prompt: "fix helper", Dir: t.TempDir(), Tier: adapter.TierNormal})
	return store.Record{Status: store.Completed, Verification: "verified", Tier: "easy", ExecutionTier: "normal", FeatureSchema: featureSchema, ClassifyFeatures: f[:], Prompt: "PROMPT_MUST_NOT_BE_SAVED"}
}

func TestPersistenceResetAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "classifier.json")
	r := observedRecord(t)
	for range MinSamples {
		if err := Observe(path, r); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Load(path)
	if err != nil || m.Tiers[adapter.TierNormal].Samples != MinSamples || m.Tiers[adapter.TierEasy].Samples != 0 {
		t.Fatalf("%+v %v", m, err)
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(b), "PROMPT_MUST_NOT_BE_SAVED") || strings.Contains(string(b), "prompt") {
		t.Fatalf("prompt persisted: %s %v", b, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatal("model is not private")
	}
	m.FeatureSchema++
	obsolete, _ := json.Marshal(m)
	for _, content := range []string{string(obsolete), "{broken", `{"featureSchema":1,"tiers":{}}`, `{"featureSchema":1,"tiers":{"easy":{"weights":[0]},"normal":{},"hard":{}}}`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(path)
		if err != nil || !reflect.DeepEqual(loaded, Prior()) {
			t.Fatalf("invalid model did not reset: %+v %v", loaded, err)
		}
		if err := Observe(path, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := Reset(path); err != nil {
		t.Fatal(err)
	}
	got, _ := Load(path)
	if !reflect.DeepEqual(got, Prior()) {
		t.Fatal("reset failed")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".classifier-") {
			t.Fatal("temporary file left behind")
		}
	}
}

func TestIgnoredObservationsDoNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "classifier.json")
	for _, mutate := range []func(*store.Record){
		func(r *store.Record) { r.ExitClass = "rate_limit" },
		func(r *store.Record) { r.FeatureSchema++ },
		func(r *store.Record) { r.ClassifyFeatures = nil },
		func(r *store.Record) { r.ExecutionTier = "" },
		func(r *store.Record) { r.Verification = "unverified" },
	} {
		r := observedRecord(t)
		mutate(&r)
		if err := Observe(path, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("ignored observations wrote state")
	}
}

func TestConcurrentAtomicUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "classifier.json")
	r := observedRecord(t)
	if err := Reset(path); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := Observe(path, r); err != nil {
				t.Error(err)
			}
		})
	}
	for range 30 {
		b, err := os.ReadFile(path)
		if err != nil || !json.Valid(b) {
			t.Fatalf("non-atomic JSON: %s %v", b, err)
		}
		if m, err := Load(path); err != nil || !m.valid() {
			t.Fatalf("partial model visible: %+v %v", m, err)
		}
	}
	wg.Wait()
	m, err := Load(path)
	if err != nil || m.Tiers[adapter.TierNormal].Samples != 8 {
		t.Fatalf("lost updates: %+v %v", m, err)
	}
}
