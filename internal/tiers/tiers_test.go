package tiers

import (
	"os"
	"path/filepath"
	"strings"
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

func TestSpeedAndStrength(t *testing.T) {
	table, err := parse([]byte(`{"tiers":{"easy":[{"agent":"codex","model":"gpt-small","speed":5,"strength":1}]}}`))
	if err != nil || table.Tiers[adapter.TierEasy][0].Speed != 5 || table.Tiers[adapter.TierEasy][0].Strength != 1 {
		t.Fatal(err, table)
	}
	if len(table.FieldProblems()) != 0 {
		t.Fatal(table.FieldProblems())
	}
	for _, body := range []string{
		`{"tiers":{"easy":[{"agent":"codex","model":"gpt-small","speed":6}]}}`,
		`{"tiers":{"easy":[{"agent":"codex","model":"gpt-small","strength":-1}]}}`,
	} {
		if _, err := parse([]byte(body)); err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatal("accepted", body, err)
		}
	}
	omitted, err := parse([]byte(`{"tiers":{"easy":[{"agent":"codex","model":"gpt-small"}]}}`))
	if err != nil || omitted.Tiers[adapter.TierEasy][0].Speed != 0 || omitted.Tiers[adapter.TierEasy][0].Strength != 0 {
		t.Fatal(err, omitted)
	}
	hand := Table{Tiers: map[adapter.Tier][]Entry{adapter.TierHard: {{Agent: "a", Model: "m", Speed: 9, Strength: 0}}}}
	if len(hand.FieldProblems()) != 1 {
		t.Fatal(hand.FieldProblems())
	}
}
