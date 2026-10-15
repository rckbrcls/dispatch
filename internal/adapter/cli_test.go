package adapter

import (
	"reflect"
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
	}
	if claudeModel.MatchString("grok-4.7-low") {
		t.Error("grok must pass")
	}
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
