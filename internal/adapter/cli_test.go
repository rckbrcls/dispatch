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
