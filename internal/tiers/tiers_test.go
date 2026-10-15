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
