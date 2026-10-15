package tiers

import (
	"os"
	"path/filepath"
	"testing"

	"dispatch/internal/adapter"
)

func TestDefaultCoversAllTiers(t *testing.T) {
	d := Default()
	for _, tier := range []adapter.Tier{adapter.TierEasy, adapter.TierNormal, adapter.TierHard} {
		if len(d.Tiers[tier]) == 0 {
			t.Errorf("tier %s empty", tier)
		}
	}
}

func TestLoadMissingAndInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "none.json")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "bad.json")
	os.WriteFile(p, []byte(`{"tiers":{"wild":[{"agent":"a","model":"m"}]}}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for unknown tier")
	}
}

func TestProviderEffortsCompatibility(t *testing.T) {
	table, err := parse([]byte(`{"tiers":{"easy":[{"agent":"codex","model":"gpt-small","efforts":["low","high"]}]}}`))
	if err != nil || table.Tiers[adapter.TierEasy][0].Provider != "openai" {
		t.Fatal(table, err)
	}
	for _, body := range []string{
		`{"tiers":{"easy":[{"agent":"codex","model":"gpt-small","efforts":["wild"]}]}}`,
		`{"tiers":{"easy":[{"agent":"codex","model":"gpt-small","provider":"google"}]}}`,
	} {
		if _, err := parse([]byte(body)); err == nil {
			t.Fatal("accepted invalid metadata", body)
		}
	}
	table.Tiers[adapter.TierEasy][0].Agent = "grok"
	if len(table.CapabilityProblems(adapter.Known())) == 0 {
		t.Fatal("effort accepted for CLI without control")
	}
}
