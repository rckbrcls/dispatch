package config

import (
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
