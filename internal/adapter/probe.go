package adapter

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"dispatch/internal/sandbox"
)

const probeTimeout = 3 * time.Second

// ProbeStatus is the doctor verdict for one installed CLI.
type ProbeStatus string

const (
	StatusOK         ProbeStatus = "OK"
	StatusDegraded   ProbeStatus = "DEGRADED"
	StatusUnverified ProbeStatus = "UNVERIFIED"
)

// FlagCheck is one flag the adapter passes, as seen in --help.
type FlagCheck struct {
	Name       string `json:"flag"`
	Present    bool   `json:"present"`
	Capability string `json:"capability,omitempty"`
}

// ProbeReport is the result of probing one CLI. Probe never runs a model.
type ProbeReport struct {
	Name         string       `json:"name"`
	Binary       string       `json:"binary"`
	Version      string       `json:"version,omitempty"`
	Flags        []FlagCheck  `json:"flags"`
	Capabilities Capabilities `json:"capabilities"`
	Status       ProbeStatus  `json:"status"`
	Notes        []string     `json:"notes,omitempty"`
	Installed    bool         `json:"installed"`
}

// probeFlag is a flag or subcommand token the adapter puts on the command line.
// Capability, when set, is required by runs that need that capability.
type probeFlag struct {
	Name       string
	Capability string
	// Sub, when set, names the subcommand whose --help lists this flag
	// (e.g. "exec" for `codex exec --help`); empty means the top-level help.
	Sub string
}

var versionRE = regexp.MustCompile(`v?(\d+\.\d+(?:\.\d+)?)`)

// Doctor probes every known adapter whose binary is on PATH.
// A missing CLI is omitted. Probe failures become a status, not an error.
func Doctor(ctx context.Context) []ProbeReport {
	out := []ProbeReport{}
	for _, a := range Known() {
		if _, err := exec.LookPath(a.Binary()); err != nil {
			continue
		}
		p, ok := a.(Prober)
		if !ok {
			continue
		}
		r := p.Probe(ctx)
		r.Installed = true
		out = append(out, r)
	}
	return out
}

func probeBinary(ctx context.Context, name, binary string, caps Capabilities, flags []probeFlag) ProbeReport {
	rep := ProbeReport{
		Name: name, Binary: binary, Capabilities: caps,
		Flags: []FlagCheck{}, Status: StatusUnverified,
	}
	if _, err := exec.LookPath(binary); err != nil {
		rep.Notes = []string{binary + " not on PATH"}
		return rep
	}
	rep.Installed = true

	verOut, verErr := probeOutput(ctx, binary, "--version")
	ver := ""
	if m := versionRE.FindStringSubmatch(verOut); len(m) == 2 {
		ver = m[1]
	}
	rep.Version = ver
	versionOK := ver != ""
	if !versionOK {
		note := binary + " --version: version unknown"
		if verErr != nil {
			note = oneLine(fmt.Errorf("%s --version: %w", binary, verErr))
		}
		rep.Notes = append(rep.Notes, note)
	}

	helpOut, helpErr := probeOutput(ctx, binary, "--help")
	helpOK := helpErr == nil && strings.TrimSpace(helpOut) != ""
	if !helpOK {
		note := binary + " --help: no response"
		if helpErr != nil {
			note = oneLine(fmt.Errorf("%s --help: %w", binary, helpErr))
		}
		rep.Notes = append(rep.Notes, note)
	}

	var missing []string
	subHelp := map[string]string{"": helpOut}
	subOK := map[string]bool{"": helpOK}
	for _, f := range flags {
		if _, seen := subHelp[f.Sub]; !seen {
			out, err := probeOutput(ctx, binary, f.Sub, "--help")
			subHelp[f.Sub] = out
			subOK[f.Sub] = err == nil && strings.TrimSpace(out) != ""
			if !subOK[f.Sub] {
				rep.Notes = append(rep.Notes, binary+" "+f.Sub+" --help: no response")
				helpOK = false
			}
		}
		ok := subOK[f.Sub]
		present := ok && flagPresent(subHelp[f.Sub], f.Name)
		rep.Flags = append(rep.Flags, FlagCheck{Name: f.Name, Present: present, Capability: f.Capability})
		if ok && !present {
			missing = append(missing, f.Name)
		}
	}
	if len(missing) > 0 {
		rep.Notes = append(rep.Notes, "missing flags: "+strings.Join(missing, ", "))
	}
	switch {
	case !versionOK || !helpOK:
		rep.Status = StatusUnverified
	case len(missing) > 0:
		rep.Status = StatusDegraded
	default:
		rep.Status = StatusOK
		rep.Notes = nil
	}
	for i, n := range rep.Notes {
		rep.Notes[i] = sandbox.Redact(n)
	}
	return rep
}

func probeOutput(ctx context.Context, binary string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, binary, args...)
	cmd.Env = sandbox.Environ()
	ownGroup(cmd)
	out, err := cmd.CombinedOutput()
	text := string(out)
	if len(text) > 256*1024 {
		text = text[:256*1024]
	}
	if err != nil {
		if cctx.Err() != nil && ctx.Err() == nil {
			return text, fmt.Errorf("timed out")
		}
		return text, err
	}
	return text, nil
}

func flagPresent(help, flag string) bool {
	if flag == "" {
		return false
	}
	rest := help
	for {
		i := strings.Index(rest, flag)
		if i < 0 {
			return false
		}
		abs := len(help) - len(rest) + i
		leftOK := abs == 0 || isFlagBoundary(help[abs-1])
		end := abs + len(flag)
		rightOK := end == len(help) || !isFlagTokenChar(help[end])
		if leftOK && rightOK {
			return true
		}
		rest = rest[i+len(flag):]
	}
}

func isFlagBoundary(b byte) bool {
	switch b {
	case ' ', '\n', '\t', '\r', '[', ']', '(', ')', ',', '|', '=', ':', '<', '>', '"', '\'', '`':
		return true
	default:
		return false
	}
}

func isFlagTokenChar(b byte) bool {
	return b == '-' || b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func oneLine(err error) string {
	s := strings.TrimSpace(err.Error())
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
