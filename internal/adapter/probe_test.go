package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlagBoundaries(t *testing.T) {
	cases := []struct {
		help, flag string
		want       bool
	}{
		{"  --mode ask\n  --model x\n", "--mode", true},
		{"  --mode ask\n  --model x\n", "--model", true},
		{"  --model x\n", "--mode", false},
		{"  --json-removed\n", "--json", false},
		{"  --json\n", "--json", true},
		{"  --output-format json\n", "-o", false},
		{"  --output-format json\n", "--output-format", true},
		{"execution", "exec", false},
		{"  exec resume\n", "exec", true},
		{"  exec resume\n", "resume", true},
		{"  -p\n", "-p", true},
		{"  --permission-mode plan\n", "-p", false},
		{"  --permission-mode plan\n", "--permission-mode", true},
	}
	for _, c := range cases {
		if got := flagPresent(c.help, c.flag); got != c.want {
			t.Errorf("flagPresent(%q, %q) = %v", c.help, c.flag, got)
		}
	}
}

func TestGoldenProbes(t *testing.T) {
	byName := map[string]Prober{}
	bins := map[string]string{}
	for _, a := range Known() {
		p, ok := a.(Prober)
		if !ok {
			t.Fatalf("%s is not a Prober", a.Name())
		}
		byName[a.Name()] = p
		bins[a.Name()] = a.Binary()
	}
	roots, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) == 0 {
		t.Fatal("no golden adapters")
	}
	seen := map[string]bool{}
	for _, ent := range roots {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		seen[name] = true
		versions, err := os.ReadDir(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, ver := range versions {
			if !ver.IsDir() {
				continue
			}
			name, verName := name, ver.Name()
			t.Run(name+"/"+verName, func(t *testing.T) {
				base := filepath.Join("testdata", name, verName)
				verText := mustRead(t, filepath.Join(base, "version.txt"))
				helpText := mustRead(t, filepath.Join(base, "help.txt"))
				logPath := installProbeCLI(t, t.TempDir(), bins[name], verText, helpText)
				t.Setenv("PATH", filepath.Dir(logPath))
				// installProbeCLI puts the binary next to the log, in its dir.
				r := byName[name].Probe(context.Background())
				if r.Status != StatusOK || r.Version != verName {
					t.Fatalf("status %s version %q notes %v flags %+v", r.Status, r.Version, r.Notes, r.Flags)
				}
				assertProbeOnly(t, logPath)
			})
		}
	}
	for _, a := range Known() {
		if !seen[a.Name()] {
			t.Errorf("missing testdata for %s", a.Name())
		}
	}
}

func TestProbeDegradedWhenFlagRemoved(t *testing.T) {
	help := mustRead(t, "testdata/codex/0.99.0/help.txt")
	ver := mustRead(t, "testdata/codex/0.99.0/version.txt")
	mutated := strings.ReplaceAll(help, "--json", "--json-removed")
	if mutated == help {
		t.Fatal("fixture has no --json to remove")
	}
	dir := t.TempDir()
	logPath := installProbeCLI(t, dir, "codex", ver, mutated)
	t.Setenv("PATH", dir)
	r := Codex{}.Probe(context.Background())
	if r.Status != StatusDegraded {
		t.Fatalf("status %s notes %v", r.Status, r.Notes)
	}
	var jsonFlag *FlagCheck
	for i := range r.Flags {
		if r.Flags[i].Name == "--json" {
			jsonFlag = &r.Flags[i]
		}
	}
	if jsonFlag == nil || jsonFlag.Present {
		t.Fatalf("flags %+v", r.Flags)
	}
	assertProbeOnly(t, logPath)
}

func TestProbeUnverified(t *testing.T) {
	help := mustRead(t, "testdata/grok/1.0.52/help.txt")
	t.Run("unknown version", func(t *testing.T) {
		dir := t.TempDir()
		installProbeCLI(t, dir, "grok", "grok dev\n", help)
		t.Setenv("PATH", dir)
		r := Grok().(Prober).Probe(context.Background())
		if r.Status != StatusUnverified || r.Version != "" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("no response", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "grok")
		script := "#!/bin/sh\nexit 1\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		r := Grok().(Prober).Probe(context.Background())
		if r.Status != StatusUnverified {
			t.Fatalf("%+v", r)
		}
	})
}

func TestProbeDoesNotLeakSecret(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "super-secret-value")
	dir := t.TempDir()
	marker := filepath.Join(dir, "env.txt")
	help := filepath.Join(dir, "help.txt")
	if err := os.WriteFile(help, []byte(mustRead(t, "testdata/grok/1.0.52/help.txt")), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ -n \"$AWS_SECRET_ACCESS_KEY\" ]; then echo LEAKED > %q; else echo clean > %q; fi\ncase \"$1\" in\n--version) echo 'grok 1.0.52' ;;\n--help) /bin/cat %q ;;\n*) echo refusing >&2; exit 97 ;;\nesac\n", marker, marker, help)
	if err := os.WriteFile(filepath.Join(dir, "grok"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	r := Grok().(Prober).Probe(context.Background())
	if r.Status != StatusOK {
		t.Fatalf("%+v", r)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "clean" {
		t.Fatalf("secret reached the CLI: %q", got)
	}
}

func TestCursorAndGrokHaveNoOfficialUsage(t *testing.T) {
	for _, a := range []Adapter{Cursor(), Grok()} {
		if a.Capabilities().Usage {
			t.Errorf("%s claims an official usage source", a.Name())
		}
		if _, ok := a.(UsageProvider); ok {
			t.Errorf("%s implements UsageProvider", a.Name())
		}
	}
	for _, a := range []Adapter{Codex{}, Antigravity{}} {
		if !a.Capabilities().Usage {
			t.Errorf("%s should claim usage", a.Name())
		}
		u, ok := a.(UsageProvider)
		if !ok || u.UsageSource() == "" {
			t.Errorf("%s missing UsageProvider", a.Name())
		}
	}
}

func TestDoctorDegraded(t *testing.T) {
	dir := t.TempDir()
	helpPath := ""
	for _, a := range Known() {
		base := filepath.Join("testdata", a.Name())
		versions, err := os.ReadDir(base)
		if err != nil || len(versions) == 0 {
			t.Fatal(a.Name(), err)
		}
		verDir := filepath.Join(base, versions[0].Name())
		hp := installProbeCLI(t, dir, a.Binary(), mustRead(t, filepath.Join(verDir, "version.txt")), mustRead(t, filepath.Join(verDir, "help.txt")))
		if a.Name() == "codex" {
			helpPath = filepath.Join(dir, "codex.help")
			_ = hp
		}
	}
	t.Setenv("PATH", dir)
	reports := Doctor(context.Background())
	if len(reports) != len(Known()) {
		t.Fatalf("reports %d", len(reports))
	}
	for _, r := range reports {
		if r.Status != StatusOK {
			t.Fatalf("%s %s %v", r.Name, r.Status, r.Notes)
		}
	}
	raw, err := os.ReadFile(helpPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helpPath, []byte(strings.ReplaceAll(string(raw), "--json", "--json-removed")), 0o644); err != nil {
		t.Fatal(err)
	}
	var codex ProbeReport
	for _, r := range Doctor(context.Background()) {
		if r.Name == "codex" {
			codex = r
		} else if r.Status != StatusOK {
			t.Fatalf("%s became %s", r.Name, r.Status)
		}
	}
	if codex.Status != StatusDegraded {
		t.Fatalf("codex %+v", codex)
	}
}

// installProbeCLI writes an sh fake that answers --version and --help only.
// It returns the invocation log path. The binary lives in dir.
func installProbeCLI(t *testing.T, dir, binary, versionText, helpText string) string {
	t.Helper()
	verPath := filepath.Join(dir, binary+".version")
	helpPath := filepath.Join(dir, binary+".help")
	logPath := filepath.Join(dir, binary+".log")
	if err := os.WriteFile(verPath, []byte(versionText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helpPath, []byte(helpText), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1" in
--version) /bin/cat %q ;;
--help) /bin/cat %q ;;
*) echo "refusing $*" >&2; exit 97 ;;
esac
`, logPath, verPath, helpPath)
	if err := os.WriteFile(filepath.Join(dir, binary), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return logPath
}

func assertProbeOnly(t *testing.T, logPath string) {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line != "--version" && line != "--help" {
			t.Fatalf("probe invoked %q\nlog:\n%s", line, b)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
