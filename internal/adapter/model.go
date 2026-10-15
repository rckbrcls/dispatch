package adapter

import "strings"

// effortSuffixes are the model-name tails that name an effort level.
var effortSuffixes = map[string]bool{
	"none": true, "minimal": true, "low": true, "medium": true,
	"high": true, "xhigh": true, "max": true,
}

// EffortOf returns the effort encoded as the last hyphen-suffix of a model
// name (gemini-3.8-flash-high → high). An empty result means the name does
// not carry one; callers leave the field absent.
func EffortOf(model string) string {
	i := strings.LastIndex(model, "-")
	if i < 0 || i+1 >= len(model) {
		return ""
	}
	suf := strings.ToLower(model[i+1:])
	if effortSuffixes[suf] {
		return suf
	}
	return ""
}

// ProviderOf derives a provider from the model name, then from the agent.
// An empty result means unknown.
func ProviderOf(agent, model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "gemini"):
		return "google"
	case strings.Contains(m, "claude"):
		return "anthropic"
	case strings.Contains(m, "grok"):
		return "xai"
	case strings.Contains(m, "composer"):
		return "cursor"
	case strings.Contains(m, "gpt-") || strings.HasPrefix(m, "gpt") || strings.Contains(m, "codex") ||
		strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3") || strings.HasPrefix(m, "o4"):
		return "openai"
	}
	switch strings.ToLower(agent) {
	case "codex":
		return "openai"
	case "grok":
		return "xai"
	case "cursor":
		return "cursor"
	case "antigravity":
		return "google"
	default:
		return ""
	}
}

func ValidEffort(e string) bool { return e == "low" || e == "medium" || e == "high" || e == "xhigh" }
func BaseModel(m string) string {
	if e := EffortOf(m); e != "" {
		return strings.TrimSuffix(m, "-"+e)
	}
	return m
}
func AllowedModel(m string) bool { return !claudeModel.MatchString(m) }
