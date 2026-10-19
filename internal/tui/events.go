package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	sCmd    = lipgloss.NewStyle().Foreground(cRun).Bold(true)
	sThink  = lipgloss.NewStyle().Foreground(cDim).Italic(true)
	sSay    = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
	sGood   = lipgloss.NewStyle().Foreground(cOK)
	sBad    = lipgloss.NewStyle().Foreground(cBad)
	sFile   = lipgloss.NewStyle().Foreground(cWarn)
	sMarker = lipgloss.NewStyle().Foreground(cAccent)
)

const maxOutputLines = 60

func mstr(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func lines(prefix, s string, st lipgloss.Style) []string {
	var out []string
	for i, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		p := prefix
		if i > 0 {
			p = strings.Repeat(" ", lipgloss.Width(prefix))
		}
		out = append(out, st.Render(p+l))
	}
	return out
}

// pretty turns one JSONL agent event (codex --json style) into a readable,
// verbose transcript: reasoning, messages, commands with their output, file
// changes and errors. Non-JSON lines pass through unchanged.
func pretty(line string) []string {
	l, _ := prettyKind(line)
	return l
}

// prettyKind also reports what the event was, so prettyAll can group a
// command with its result and separate everything else with a blank line.
func prettyKind(line string) ([]string, string) {
	out := prettyRaw(line)
	kind := "other"
	if strings.Contains(line, `"type":"item.started"`) && strings.Contains(line, `"command_execution"`) {
		kind = "cmd-start"
	} else if strings.Contains(line, `"type":"item.completed"`) && strings.Contains(line, `"command_execution"`) {
		kind = "cmd-done"
	} else if strings.Contains(line, `"type":"tool_call"`) && !strings.Contains(line, `"subtype":"completed"`) {
		kind = "cmd-start"
	} else if strings.Contains(line, `"type":"tool_call`) {
		kind = "cmd-done"
	} else if !strings.HasPrefix(strings.TrimSpace(line), "{") {
		kind = "raw"
	}
	return out, kind
}

func prettyRaw(line string) []string {
	line = strings.TrimRight(line, "\r\n ")
	if strings.HasPrefix(line, "── dispatch:") {
		return []string{sFile.Bold(true).Render(line)}
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "{") {
		return []string{dim.Render(line)}
	}
	var ev map[string]any
	if json.Unmarshal([]byte(line), &ev) != nil || mstr(ev, "type") == "" {
		return []string{dim.Render(line)}
	}
	if out, ok := prettyStream(ev); ok {
		return out
	}
	item, _ := ev["item"].(map[string]any)
	switch typ := mstr(ev, "type"); typ {
	case "thread.started":
		return nil
	case "turn.started":
		return []string{sMarker.Render("▸ turn started")}
	case "turn.completed":
		u, _ := ev["usage"].(map[string]any)
		in, _ := u["input_tokens"].(float64)
		out, _ := u["output_tokens"].(float64)
		cached, _ := u["cached_input_tokens"].(float64)
		return []string{sMarker.Render(fmt.Sprintf("■ turn completed · %s tokens in (%s cached) · %s out", kfmt(in), kfmt(cached), kfmt(out)))}
	case "turn.failed":
		e, _ := ev["error"].(map[string]any)
		return lines("✗ turn failed: ", mstr(e, "message"), sBad)
	case "error":
		return lines("⚠ ", mstr(ev, "message"), sBad)
	case "item.started":
		switch mstr(item, "type") {
		case "command_execution":
			return lines("▶ $ ", unwrapShell(mstr(item, "command")), sCmd)
		case "web_search":
			return []string{sCmd.Render("▶ search: " + mstr(item, "query"))}
		}
		return nil
	case "item.completed":
		switch t := mstr(item, "type"); t {
		case "agent_message":
			return lines("● ", mstr(item, "text"), sSay)
		case "reasoning":
			return lines("◌ ", mstr(item, "text"), sThink)
		case "command_execution":
			var out []string
			exit, known := item["exit_code"].(float64)
			o := strings.TrimRight(mstr(item, "aggregated_output"), "\n")
			n := 0
			if o != "" {
				n = strings.Count(o, "\n") + 1
			}
			info := ""
			if n > 0 {
				info = fmt.Sprintf(" · %d lines", n)
			}
			status := sGood.Render("  ✓ exit 0" + info)
			if known && exit != 0 {
				status = sBad.Render(fmt.Sprintf("  ✗ exit %d%s", int(exit), info))
			} else if !known {
				status = dim.Render("  · no exit code" + info)
			}
			out = append(out, status)
			if o != "" {
				ls := strings.Split(o, "\n")
				if len(ls) > maxOutputLines {
					ls = append(ls[:maxOutputLines], fmt.Sprintf("… +%d lines", len(ls)-maxOutputLines))
				}
				for _, l := range ls {
					out = append(out, dim.Render("  │ "+l))
				}
			}
			return out
		case "file_change":
			var out []string
			cs, _ := item["changes"].([]any)
			for _, c := range cs {
				m, _ := c.(map[string]any)
				out = append(out, sFile.Render(fmt.Sprintf("± %s %s", mstr(m, "kind"), mstr(m, "path"))))
			}
			return out
		case "error":
			return lines("⚠ ", mstr(item, "message"), dim)
		case "web_search":
			return []string{sCmd.Render("✓ search: " + mstr(item, "query"))}
		default:
			b, _ := json.Marshal(item)
			return []string{dim.Render("· " + t + " " + oneLine(string(b), 300))}
		}
	}
	return nil
}

// prettyAll renders a whole transcript: events separated by a blank line, a
// command kept together with its result, and repeated raw lines (such as the
// same config warning on every turn) shown once.
func prettyAll(ls []string) []string {
	ls = mergeDeltas(ls)
	var out []string
	seen := map[string]bool{}
	prev := ""
	for _, l := range ls {
		if strings.TrimSpace(l) == "" {
			continue
		}
		lines, kind := prettyKind(l)
		if len(lines) == 0 {
			continue
		}
		if key := strings.Join(lines, "\n"); kind == "raw" || strings.HasPrefix(ansiStrip(key), "⚠") {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		joined := !(kind == "cmd-done" && prev == "cmd-start") && !(kind == "raw" && prev == "raw")
		if len(out) > 0 && joined {
			out = append(out, "")
		}
		out = append(out, lines...)
		prev = kind
	}
	return out
}

// unwrapShell turns `/bin/zsh -lc 'cmd'` into `cmd`.
func ansiStrip(s string) string { return ansi.Strip(s) }

func unwrapShell(c string) string {
	for _, p := range []string{"/bin/zsh -lc ", "/bin/bash -lc ", "/bin/sh -lc ", "zsh -lc ", "bash -lc ", "sh -lc "} {
		if strings.HasPrefix(c, p) {
			r := strings.TrimSpace(strings.TrimPrefix(c, p))
			if len(r) >= 2 && (r[0] == '\'' || r[0] == '"') && r[len(r)-1] == r[0] {
				r = r[1 : len(r)-1]
			}
			return r
		}
	}
	return c
}

// kfmt formats a token count compactly: 750, 12.3k, 1.2M.
func kfmt(f float64) string {
	switch {
	case f >= 1e6:
		return fmt.Sprintf("%.1fM", f/1e6)
	case f >= 1e4:
		return fmt.Sprintf("%.1fk", f/1e3)
	}
	return fmt.Sprint(int(f))
}
