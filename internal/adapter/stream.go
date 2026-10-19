package adapter

import (
	"encoding/json"
	"strings"
)

// Cursor and Grok run in streaming JSON so the live log shows the agent's
// reasoning, tool calls and reply as they happen. The parsers below rebuild
// the final reply and session id from the event stream once the run ends.

// parseCursorStream reads `cursor-agent -p --output-format stream-json`.
// The "result" event carries the whole reply; without it (a run that died
// mid-turn) the last full assistant message is used. Partial deltas carry a
// timestamp_ms and are skipped here.
func parseCursorStream(stdout string) (string, string) {
	var result, lastFull, session string
	found := false
	for _, l := range lines(stdout) {
		l = strings.TrimSpace(l)
		if l == "" || l[0] != '{' {
			continue
		}
		var ev struct {
			Type        string `json:"type"`
			SessionID   string `json:"session_id"`
			Result      string `json:"result"`
			TimestampMs *int64 `json:"timestamp_ms"`
			Message     struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(l), &ev) != nil || ev.Type == "" {
			continue
		}
		found = true
		if ev.SessionID != "" {
			session = ev.SessionID
		}
		switch ev.Type {
		case "result":
			result = ev.Result
		case "assistant":
			if ev.TimestampMs != nil {
				continue
			}
			var b strings.Builder
			for _, c := range ev.Message.Content {
				if c.Type == "text" {
					b.WriteString(c.Text)
				}
			}
			if strings.TrimSpace(b.String()) != "" {
				lastFull = b.String()
			}
		}
	}
	if !found {
		return strings.TrimSpace(stdout), ""
	}
	if strings.TrimSpace(result) == "" {
		result = lastFull
	}
	return strings.TrimSpace(result), session
}

// parseGrokStream reads `grok --output-format streaming-json`. The reply
// arrives as "text" deltas; text written before a tool call is narration, so
// only the text after the last tool call is the answer (or the last
// narration, when the turn ended on a tool call). "end" carries the session.
func parseGrokStream(stdout string) (string, string) {
	var reply strings.Builder
	session, prev := "", ""
	found := false
	for _, l := range lines(stdout) {
		l = strings.TrimSpace(l)
		if l == "" || l[0] != '{' {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			Data      string `json:"data"`
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal([]byte(l), &ev) != nil || ev.Type == "" {
			continue
		}
		found = true
		switch ev.Type {
		case "text":
			reply.WriteString(ev.Data)
		case "tool_call":
			if strings.TrimSpace(reply.String()) != "" {
				prev = reply.String()
			}
			reply.Reset()
		case "end":
			session = ev.SessionID
		}
	}
	if !found {
		return strings.TrimSpace(stdout), ""
	}
	if strings.TrimSpace(reply.String()) == "" {
		return strings.TrimSpace(prev), session
	}
	return strings.TrimSpace(reply.String()), session
}
