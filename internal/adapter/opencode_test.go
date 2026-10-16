package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseOpenCodeJSON(t *testing.T) {
	out := `{"type":"step_start","sessionID":"ses_abc","part":{"type":"step-start"}}
{"type":"text","sessionID":"ses_abc","part":{"type":"text","text":"pong"}}
not json
{"type":"text","sessionID":"ses_abc","part":{"type":"text","text":"second"}}
`
	text, session := parseOpenCodeJSON(out)
	if text != "pong\nsecond" || session != "ses_abc" {
		t.Fatalf("%q %q", text, session)
	}
}

func TestOpenCodeArgsAndProvider(t *testing.T) {
	a := OpenCode().(cliAdapter)
	fresh := strings.Join(a.args(Request{Prompt: "p"}, "opencode/beam-free", "", false), " ")
	if !strings.Contains(fresh, "run --standalone --format json --auto") || strings.Contains(fresh, "--session") || !strings.HasSuffix(fresh, "--model opencode/beam-free p") {
		t.Fatal(fresh)
	}
	if !strings.Contains(strings.Join(a.args(Request{Prompt: "p"}, "m/x", "ses_1", true), " "), "--session ses_1") {
		t.Fatal("resume must pass the session")
	}
	if a.caps.ReadOnly {
		t.Fatal("opencode has no read-only mode")
	}
	if ProviderOf("opencode", "openrouter/some-model") != "openrouter" {
		t.Fatal(ProviderOf("opencode", "openrouter/some-model"))
	}
	if got := parseOpenCodeModels("opencode/a\nnoise line\nx/y\n"); len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

func TestBinaryOverrideAndDetect(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "my-opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Cleanup(func() { SetBinaries(nil) })

	if Bin("opencode") != "opencode" {
		t.Fatal("default binary expected")
	}
	SetBinaries(map[string]string{"opencode": bin})
	if OpenCode().Binary() != bin || Bin("codex") != "codex" {
		t.Fatalf("override not applied: %s", OpenCode().Binary())
	}
	var got Detection
	for _, d := range Detect() {
		if d.Agent == "opencode" {
			got = d
		}
	}
	if got.Source != "configured" || got.Path != bin {
		t.Fatalf("%+v", got)
	}
	SetBinaries(map[string]string{"opencode": filepath.Join(dir, "gone")})
	for _, d := range Detect() {
		if d.Agent == "opencode" && d.Source != "invalid" {
			t.Fatalf("%+v", d)
		}
	}
	if ValidBinary("relative/path") == nil || ValidBinary(filepath.Join(dir, "gone")) == nil || ValidBinary(bin) != nil {
		t.Fatal("ValidBinary")
	}
}
