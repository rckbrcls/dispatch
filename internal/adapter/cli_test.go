package adapter

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParsers(t *testing.T) {
	if got := parseCursorModels("Available models\n\nauto - Auto (default)\ngrok-4.7-low - Grok 4.7  Low\n"); !reflect.DeepEqual(got, []string{"auto", "grok-4.7-low"}) {
		t.Errorf("cursor: %v", got)
	}
	if got := parseAgyModels("Fetching available models...\ngemini-3.8-flash-low\tGemini 3.8 Flash (Low)\n"); !reflect.DeepEqual(got, []string{"gemini-3.8-flash-low"}) {
		t.Errorf("agy: %v", got)
	}
	if got := parseGrokModels("Default model: grok-4.7\n\nAvailable models:\n  * grok-4.7 (default)\n  - grok-4.6\n"); !reflect.DeepEqual(got, []string{"grok-4.7", "grok-4.6"}) {
		t.Errorf("grok: %v", got)
	}
}

func TestClaudeFilter(t *testing.T) {
	for _, id := range []string{"claude-opus-5-5-low", "claude-fable-5-thinking-high", "x-Sonnet-y"} {
		if !claudeModel.MatchString(id) {
			t.Errorf("%s should be filtered", id)
		}
		for _, agent := range []string{"antigravity", "cursor", "codex", "grok", "opencode", "unknown"} {
			if got := AllowedModel(agent, id); got != (agent == "antigravity") {
				t.Errorf("AllowedModel(%q, %q) = %v", agent, id, got)
			}
			if got := ProviderOf(agent, id); got != "anthropic" {
				t.Errorf("ProviderOf(%q, %q) = %q", agent, id, got)
			}
		}
	}
	if claudeModel.MatchString("grok-4.7-low") {
		t.Error("grok must pass")
	}
}

func installModelCLI(t *testing.T, agent, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	SetBinaries(map[string]string{agent: path})
	t.Cleanup(func() { SetBinaries(nil) })
}

func TestModelDiscoveryClaudePolicy(t *testing.T) {
	for _, tt := range []struct {
		a      Adapter
		script string
		want   []Model
	}{
		{Antigravity{}, "[ \"$1\" = models ] || exit 99\nprintf 'gemini-fast-low\tGemini\nclaude-opus-high\tClaude\n'\n", []Model{{ID: "gemini-fast-low"}, {ID: "claude-opus-high"}}},
		{Cursor(), "printf 'gpt-small - GPT\nclaude-opus-high - Claude\n'\n", []Model{{ID: "gpt-small"}}},
		{Grok(), "printf -- '- grok-fast\n- claude-opus-high\n'\n", []Model{{ID: "grok-fast"}}},
		{OpenCode(), "printf 'openai/gpt-small\nanthropic/claude-opus-high\n'\n", []Model{{ID: "openai/gpt-small"}}},
		{Codex{}, "read -r init\nread -r notification\nread -r request\nprintf '%s\\n' '{\"id\":2,\"result\":{\"data\":[{\"model\":\"gpt-small\"},{\"id\":\"claude-opus-high\"}]}}'\n", []Model{{ID: "gpt-small"}}},
	} {
		t.Run(tt.a.Name(), func(t *testing.T) {
			installModelCLI(t, tt.a.Name(), tt.script)
			got, err := tt.a.Models(context.Background())
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("models = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
	t.Run("antigravity empty", func(t *testing.T) {
		installModelCLI(t, "antigravity", "[ \"$1\" = models ] || exit 99\nprintf 'Fetching available models...\\n'\n")
		got, err := (Antigravity{}).Models(context.Background())
		if err != nil || len(got) != 0 {
			t.Fatal(got, err)
		}
	})
}

func TestRunClaudePolicy(t *testing.T) {
	for _, a := range []Adapter{Cursor(), Codex{}, Grok(), OpenCode()} {
		t.Run(a.Name(), func(t *testing.T) {
			installModelCLI(t, a.Name(), "exit 99\n")
			_, err := a.Run(context.Background(), Request{}, "claude-opus-high")
			if err == nil || err.Error() != a.Name()+": claude models are not routed through this CLI" {
				t.Fatal(err)
			}
		})
	}
	t.Run("antigravity", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "args")
		t.Setenv("DISPATCH_TEST_ARGS", path)
		installModelCLI(t, "antigravity", "printf '%s\\n' \"$@\" > \"$DISPATCH_TEST_ARGS\"\nprintf '%s\\n' '{\"conversation_id\":\"c-1\",\"response\":\"ok\"}'\n")
		r, err := (Antigravity{}).Run(context.Background(), Request{Prompt: "test"}, "claude-opus-high")
		if err != nil || r.Output != "ok" || r.SessionID != "c-1" {
			t.Fatal(r, err)
		}
		args, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(args), "--model\nclaude-opus-high\n") || !strings.HasSuffix(string(args), "-p\ntest\n") {
			t.Fatalf("args = %q, %v", args, err)
		}
	})
}

func TestParseAgyUsage(t *testing.T) {
	raw := []byte(`{"command":{"data":{"groups":[
	 {"name":"Gemini Models","buckets":[
	  {"window":"weekly","remaining_fraction":0.97,"reset_time":"2026-10-20T18:45:17Z"},
	  {"window":"5h","remaining_fraction":1,"reset_time":"2026-10-15T08:05:33Z"}]},
	 {"name":"Claude and GPT models","buckets":[
	  {"window":"weekly","remaining_fraction":0.5,"reset_time":"2026-10-22T03:17:31Z"},
	  {"window":"5h","remaining_fraction":0.05,"reset_time":"2026-10-15T08:17:31Z"}]}]}}}`)
	g, err := parseAgyUsage(raw)
	if err != nil {
		t.Fatal(err)
	}
	if q := g["Gemini Models"]; q.WindowMinutes != 10080 || q.UsedPercent < 2.9 || q.UsedPercent > 3.1 {
		t.Errorf("gemini: %+v", q)
	}
	if q := g["Claude and GPT models"]; q.WindowMinutes != 300 {
		t.Errorf("3p should pick the exhausted 5h bucket: %+v", q)
	}
}

func TestParseAgyJSON(t *testing.T) {
	out, sid := parseAgyJSON(`{"conversation_id":"c-1","status":"SUCCESS","response":" hi \n"}`)
	if out != "hi" || sid != "c-1" {
		t.Errorf("%q %q", out, sid)
	}
}

func TestCodexThreadID(t *testing.T) {
	if got := codexThreadID([]byte("noise\n{\"type\":\"thread.started\",\"thread_id\":\"t-9\"}\n")); got != "t-9" {
		t.Errorf("got %q", got)
	}
}

func TestNewUUID(t *testing.T) {
	u := newUUID()
	if len(u) != 36 || u[14] != '4' {
		t.Errorf("bad uuid %s", u)
	}
}

func TestEffortApplication(t *testing.T) {
	args := codexArgs(Request{Effort: "xhigh"}, "gpt-large", "result.txt")
	found := false
	for i, arg := range args {
		if arg == "-c" && i+1 < len(args) && args[i+1] == `model_reasoning_effort="xhigh"` {
			found = true
		}
	}
	if !found {
		t.Fatal(args)
	}
	if (Codex{}).Capabilities().EffortMode != "config" {
		t.Fatal("codex effort control missing")
	}
	if (Antigravity{}).Capabilities().EffortMode != "model-suffix" {
		t.Fatal("agy effort control missing")
	}
	for _, a := range []Adapter{Cursor(), Grok()} {
		if a.Capabilities().EffortMode != "" {
			t.Fatal("unverified independent effort control", a.Name())
		}
	}
}

func TestParseCursorStream(t *testing.T) {
	b, err := os.ReadFile("testdata/cursor/stream.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	out, session := parseCursorStream(string(b))
	if out != "O arquivo `a.txt` contém apenas a palavra “hello”." || session != "c943abc3-0729-419a-a828-b5b3aec75982" {
		t.Fatalf("%q %q", out, session)
	}
	// A run that died before "result" keeps the last full message, not deltas.
	cut := string(b[:strings.LastIndex(string(b), `{"type":"result"`)])
	if out, _ := parseCursorStream(cut); out != "O arquivo `a.txt` contém apenas a palavra “hello”." {
		t.Fatalf("no result: %q", out)
	}
	if out, session := parseCursorStream("plain text reply\n"); out != "plain text reply" || session != "" {
		t.Fatalf("non-JSON: %q %q", out, session)
	}
}

func TestParseGrokStream(t *testing.T) {
	b, err := os.ReadFile("testdata/grok/stream.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	out, session := parseGrokStream(string(b))
	if out != "The file says hello." || session != "3de270cb-e48b-467c-88f2-f78d2eca2b97" {
		t.Fatalf("%q %q", out, session)
	}
	narr := `{"type":"text","data":"Let me check."}` + "\n" + `{"type":"tool_call","title":"read_file"}` + "\n" + `{"type":"text","data":"Done."}` + "\n"
	if out, _ := parseGrokStream(narr); out != "Done." {
		t.Fatalf("narration leaked: %q", out)
	}
	endsOnTool := `{"type":"text","data":"Edited it."}` + "\n" + `{"type":"tool_call","title":"write"}` + "\n"
	if out, _ := parseGrokStream(endsOnTool); out != "Edited it." {
		t.Fatalf("ends on tool: %q", out)
	}
}
