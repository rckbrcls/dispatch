package tui

import (
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func renderFixture(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return ansi.Strip(strings.Join(prettyAll(strings.Split(strings.TrimRight(string(b), "\n"), "\n")), "\n"))
}

func TestPrettyCursorStream(t *testing.T) {
	got := renderFixture(t, "../adapter/testdata/cursor/stream.ndjson")
	for _, want := range []string{
		"▸ session started · Composer 2.5",
		"◌ Reading a.txt to prepare a one-sentence reply.",
		"▶ read /tmp/ws/a.txt\n  ✓ read",
		"● O arquivo `a.txt` contém apenas a palavra “hello”.",
		"■ turn completed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "● O arquivo"); n != 1 {
		t.Errorf("reply shown %d times:\n%s", n, got)
	}
}

func TestPrettyGrokStream(t *testing.T) {
	got := renderFixture(t, "../adapter/testdata/grok/stream.ndjson")
	for _, want := range []string{
		"▶ read_file /tmp/ws/a.txt\n  ✓ done",
		"● The file says hello.",
		"■ turn end_turn",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "available_commands") || strings.Contains(got, "signature") {
		t.Errorf("noise leaked:\n%s", got)
	}
}

func TestPartialReplyShowsWhileStreaming(t *testing.T) {
	got := ansi.Strip(strings.Join(prettyAll([]string{`{"type":"text","data":"Half"}`, `{"type":"text","data":" way"}`}), "\n"))
	if got != "● Half way" {
		t.Fatalf("%q", got)
	}
}
