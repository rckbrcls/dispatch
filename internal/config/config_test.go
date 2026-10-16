package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingAndPriority(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "config.json")
	f, err := Load(missing)
	if err != nil || f.Priority != "" {
		t.Fatal(err, f)
	}
	if err := os.WriteFile(missing, []byte(" \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, err = Load(missing); err != nil || f.Priority != "" {
		t.Fatal(err, f)
	}
	if err := os.WriteFile(missing, []byte(`{"priority":"quality"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, err = Load(missing); err != nil || f.Priority != "quality" {
		t.Fatal(err, f)
	}
	if err := os.WriteFile(missing, []byte(`{"priority":"turbo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(missing); err == nil {
		t.Fatal("accepted invalid priority")
	}
	if err := os.WriteFile(missing, []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(missing); err == nil {
		t.Fatal("accepted broken json")
	}
}

func TestLoadAllMergesProjectAndDeny(t *testing.T) {
	d := t.TempDir()
	g, p := filepath.Join(d, "global.json"), filepath.Join(d, "project.json")
	os.WriteFile(g, []byte(`{"priority":"speed","discover":true,"allow":{"providers":["openai"]},"deny":{"agents":["cursor"]}}`), 0o600)
	os.WriteFile(p, []byte(`{"priority":"quality","allow":{"models":["gpt"]},"deny":{"agents":["grok"],"providers":["google"]}}`), 0o600)
	f, err := LoadAll(g, p)
	if err != nil || f.Priority != "quality" || !f.Discover || len(f.Allow.Models) != 1 || len(f.Deny.Agents) != 2 || len(f.Deny.Providers) != 1 {
		t.Fatalf("%+v %v", f, err)
	}
}

func TestRejectsWrongKnownTypesAndWritePreservesUnknown(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	for _, b := range []string{`{"discover":"yes"}`, `{"allow":[]}`, `{"deny":{"agents":"codex"}}`} {
		if err := os.WriteFile(p, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Fatalf("accepted %s", b)
		}
	}
	if err := os.WriteFile(p, []byte(`{"future":{"x":1},"priority":"speed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(p, File{Priority: "quality", Discover: true, Allow: Rules{Providers: []string{"openai"}}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if !bytes.Contains(b, []byte(`"future"`)) || !bytes.Contains(b, []byte(`"quality"`)) {
		t.Fatalf("%s", b)
	}
}
