package verify

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// goTestEvent is one line of `go test -json`.
type goTestEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

type goTestState struct {
	tests map[string]map[string]bool // package -> failed tests
	pkg   map[string]bool            // package-level fail
}

func (s *goTestState) add(ev goTestEvent) {
	if ev.Action != "fail" || ev.Package == "" {
		return
	}
	if ev.Test != "" {
		if s.tests == nil {
			s.tests = map[string]map[string]bool{}
		}
		if s.tests[ev.Package] == nil {
			s.tests[ev.Package] = map[string]bool{}
		}
		s.tests[ev.Package][ev.Test] = true
		return
	}
	if s.pkg == nil {
		s.pkg = map[string]bool{}
	}
	s.pkg[ev.Package] = true
}

func (s *goTestState) line(b []byte) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return
	}
	var ev goTestEvent
	if json.Unmarshal(b, &ev) != nil {
		return
	}
	s.add(ev)
}

// ids returns package.Test for failed tests. A package with no test name
// is a build (or other package-level) failure; the package rollup that
// follows ordinary test failures is not listed again.
func (s *goTestState) ids() []string {
	if s == nil {
		return nil
	}
	var out []string
	for pkg, tests := range s.tests {
		for test := range tests {
			out = append(out, pkg+"."+test)
		}
	}
	for pkg := range s.pkg {
		if len(s.tests[pkg]) > 0 {
			continue
		}
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}

func parseGoTest(out string) []string {
	var st goTestState
	for _, line := range strings.Split(out, "\n") {
		st.line([]byte(line))
	}
	return st.ids()
}

// withGoTestJSON inserts -json after `go test` when parse is gotest.
func withGoTestJSON(argv []string) []string {
	if len(argv) < 2 || filepath.Base(argv[0]) != "go" || argv[1] != "test" {
		return argv
	}
	for _, a := range argv[2:] {
		if a == "-json" || strings.HasPrefix(a, "-json=") {
			return argv
		}
	}
	out := make([]string, 0, len(argv)+1)
	out = append(out, argv[0], argv[1], "-json")
	out = append(out, argv[2:]...)
	return out
}
