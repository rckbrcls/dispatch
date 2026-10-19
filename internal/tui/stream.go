package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Cursor (stream-json) and Grok (streaming-json) write one event per text
// delta. mergeDeltas folds each run of deltas into a single synthetic
// dispatch.text event so the log reads as paragraphs, and the last, still
// growing run shows the reply as it is being written.

type delta struct {
	kind string // "say" or "think"
	text string
}

func deltaOf(line string) (delta, bool) {
	l := strings.TrimSpace(line)
	if !strings.HasPrefix(l, "{") || !strings.Contains(l, `"type"`) {
		return delta{}, false
	}
	var ev struct {
		Type        string          `json:"type"`
		Subtype     string          `json:"subtype"`
		Text        string          `json:"text"`
		Data        string          `json:"data"`
		TimestampMs json.RawMessage `json:"timestamp_ms"`
		Message     struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(l), &ev) != nil {
		return delta{}, false
	}
	switch {
	case ev.Type == "thinking" && ev.Subtype == "delta": // cursor
		return delta{"think", ev.Text}, true
	case ev.Type == "assistant" && ev.TimestampMs != nil: // cursor partial
		var b strings.Builder
		for _, c := range ev.Message.Content {
			if c.Type == "text" {
				b.WriteString(c.Text)
			}
		}
		return delta{"say", b.String()}, true
	case ev.Type == "text": // grok
		return delta{"say", ev.Data}, true
	case ev.Type == "thinking" || ev.Type == "thought" || ev.Type == "reasoning": // grok
		if ev.Data != "" {
			return delta{"think", ev.Data}, true
		}
	}
	return delta{}, false
}

func mergeDeltas(ls []string) []string {
	var out []string
	var cur *delta
	lastSay := ""
	flush := func() {
		if cur == nil {
			return
		}
		if strings.TrimSpace(cur.text) != "" {
			b, _ := json.Marshal(map[string]string{"type": "dispatch.text", "kind": cur.kind, "text": cur.text})
			out = append(out, string(b))
			if cur.kind == "say" {
				lastSay = strings.TrimSpace(cur.text)
			}
		}
		cur = nil
	}
	for _, l := range ls {
		if d, ok := deltaOf(l); ok {
			if cur != nil && cur.kind != d.kind {
				flush()
			}
			if cur == nil {
				cur = &delta{kind: d.kind}
			}
			cur.text += d.text
			continue
		}
		flush()
		// Cursor repeats the whole message after its deltas.
		if lastSay != "" && strings.Contains(l, `"type":"assistant"`) && cursorText(l) == lastSay {
			continue
		}
		out = append(out, l)
	}
	flush()
	return out
}

func cursorText(line string) string {
	var ev struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	json.Unmarshal([]byte(line), &ev)
	var b strings.Builder
	for _, c := range ev.Message.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(b.String())
}

// prettyStream renders Cursor and Grok events; ok is false for any other
// event so the codex renderer can take it.
func prettyStream(ev map[string]any) (out []string, ok bool) {
	switch typ := mstr(ev, "type"); typ {
	case "dispatch.text":
		if mstr(ev, "kind") == "think" {
			return lines("◌ ", strings.TrimSpace(mstr(ev, "text")), sThink), true
		}
		return lines("● ", strings.TrimSpace(mstr(ev, "text")), sSay), true
	case "system": // cursor init
		if mstr(ev, "subtype") == "init" {
			return []string{sMarker.Render("▸ session started · " + mstr(ev, "model"))}, true
		}
		return nil, true
	case "user", "thinking", "available_commands", "usage":
		return nil, true
	case "assistant": // cursor full message (no deltas before it)
		msg, _ := ev["message"].(map[string]any)
		cs, _ := msg["content"].([]any)
		var b strings.Builder
		for _, c := range cs {
			m, _ := c.(map[string]any)
			if mstr(m, "type") == "text" {
				b.WriteString(mstr(m, "text"))
			}
		}
		return lines("● ", strings.TrimSpace(b.String()), sSay), true
	case "result": // cursor
		u, _ := ev["usage"].(map[string]any)
		in, _ := u["inputTokens"].(float64)
		cached, _ := u["cacheReadTokens"].(float64)
		outT, _ := u["outputTokens"].(float64)
		if isErr, _ := ev["is_error"].(bool); isErr {
			return lines("✗ turn failed: ", mstr(ev, "result"), sBad), true
		}
		return []string{sMarker.Render(fmt.Sprintf("■ turn completed · %s tokens in (%s cached) · %s out", kfmt(in), kfmt(cached), kfmt(outT)))}, true
	case "end": // grok
		u, _ := ev["usage"].(map[string]any)
		in, _ := u["input_tokens"].(float64)
		cached, _ := u["cache_read_input_tokens"].(float64)
		outT, _ := u["output_tokens"].(float64)
		return []string{sMarker.Render(fmt.Sprintf("■ turn %s · %s tokens in (%s cached) · %s out", coalesce(mstr(ev, "stopReason"), "ended"), kfmt(in), kfmt(cached), kfmt(outT)))}, true
	case "tool_call":
		if sub := mstr(ev, "subtype"); sub != "" { // cursor
			name, args, result := cursorTool(ev)
			if sub == "started" {
				return []string{sCmd.Render(strings.TrimSpace("▶ " + name + " " + args))}, true
			}
			if msg, failed := toolFailure(result); failed {
				return lines("  ✗ ", coalesce(msg, name+" failed"), sBad), true
			}
			return []string{sGood.Render("  ✓ " + name)}, true
		}
		input, _ := ev["rawInput"].(map[string]any) // grok
		return []string{sCmd.Render(strings.TrimSpace("▶ " + coalesce(mstr(ev, "title"), mstr(ev, "toolName")) + " " + argSummary(input)))}, true
	case "tool_call_update": // grok
		switch mstr(ev, "status") {
		case "completed":
			return []string{sGood.Render("  ✓ done")}, true
		case "failed":
			return lines("  ✗ ", coalesce(toolText(ev), "failed"), sBad), true
		}
		return nil, true
	}
	return nil, false
}

// cursorTool reads {"tool_call":{"readToolCall":{"args":{...},"result":{...}}}}.
func cursorTool(ev map[string]any) (name, args string, result map[string]any) {
	tc, _ := ev["tool_call"].(map[string]any)
	var keys []string
	for k := range tc {
		if strings.HasSuffix(k, "ToolCall") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "tool", "", nil
	}
	body, _ := tc[keys[0]].(map[string]any)
	a, _ := body["args"].(map[string]any)
	result, _ = body["result"].(map[string]any)
	return strings.TrimSuffix(keys[0], "ToolCall"), argSummary(a), result
}

func argSummary(a map[string]any) string {
	for _, k := range []string{"command", "path", "target_file", "file_path", "pattern", "globPattern", "query", "url", "targetDirectory"} {
		if s := mstr(a, k); s != "" {
			return oneLine(unwrapShell(s), 200)
		}
	}
	return ""
}

func toolFailure(result map[string]any) (string, bool) {
	for _, k := range []string{"error", "failure", "rejected"} {
		if v, ok := result[k]; ok {
			if m, ok := v.(map[string]any); ok {
				return coalesce(mstr(m, "error"), coalesce(mstr(m, "message"), mstr(m, "reason"))), true
			}
			s, _ := v.(string)
			return s, true
		}
	}
	if s, ok := result["success"].(map[string]any); ok {
		if code, ok := s["exitCode"].(float64); ok && code != 0 {
			return fmt.Sprintf("exit %d", int(code)), true
		}
	}
	return "", false
}

func toolText(ev map[string]any) string {
	cs, _ := ev["content"].([]any)
	for _, c := range cs {
		m, _ := c.(map[string]any)
		inner, _ := m["content"].(map[string]any)
		if t := mstr(inner, "text"); t != "" {
			return oneLine(t, 200)
		}
	}
	return ""
}
