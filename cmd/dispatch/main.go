package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/config"
	"dispatch/internal/hook"
	"dispatch/internal/router"
	"dispatch/internal/runner"
	"dispatch/internal/sandbox"
	"dispatch/internal/skill"
	"dispatch/internal/stats"
	"dispatch/internal/store"
	"dispatch/internal/subagent"
	"dispatch/internal/tiers"
	"dispatch/internal/update"
	"dispatch/internal/verify"
	"dispatch/internal/worktree"
)

// version is set at release time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usageText = `usage:
  dispatch version
  dispatch update [--check]
  dispatch on
  dispatch off
  dispatch status
  dispatch hook install | uninstall
  dispatch context --hook
  dispatch agents [--json]
  dispatch doctor [--json]
  dispatch tiers [--init | --check]
  dispatch config get [priority|discover|allow|deny]
  dispatch config set priority|discover <value> [--project]
  dispatch config allow|deny add|rm providers|models|agents <value...> [--project]
  dispatch route [--tier T] [--provider P] [--model M] [--effort E] [--agent A] [--priority speed|balanced|quality] [--relax] [--json]
  dispatch classify "<prompt>" [--tier T] [--dir D] [--readonly] [--json] [--explain] [--reset]
  dispatch run [--provider P] [--model M] [--effort E] [--agent A] [--priority speed|balanced|quality] [--relax] --tier easy|normal|hard [--dir D] [--readonly] [--timeout 10m]
               [--background] [--no-worktree] [--no-verify] [--json] <prompt | ->
  dispatch run --as <subagent> [flags] <prompt | ->   (profile from agents.json)
  dispatch subagents [--init | --check]
  dispatch run --resume <id> [--background] <follow-up prompt | ->
  dispatch wait <id> [--timeout 10m] [--json]
  dispatch stats [--by model|agent|provider|tier|effort|day|week|requested|resolved] [--since 7d] [--json] [--capacity]
  dispatch list [--status S] [--limit N]
  dispatch cancel <id>
  dispatch recover <id>   (ORPHANED worktree; commits preserved changes)
  dispatch verify <id>
  dispatch verify --init  (write .dispatch/verify.json if missing)
  dispatch merge <id> [--force] [--no-verify]
  dispatch clean <id> [--force]
  dispatch skill install [--agent claude,codex,cursor,grok,antigravity|all] [--force]
  dispatch skill show
  dispatch skill refresh   (rewrite already-installed skill copies)
  dispatch result <id> [--section summary|verification|files|agent|attempts|meta|output] [--offset N] [--limit N] [--json]

state lives in $DISPATCH_HOME (default ~/.dispatch): runs.jsonl, tiers.json, agents.json, config.json, classifier.json, baselines/`

func main() {
	if len(os.Args) < 2 {
		fatalUsage("")
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "on":
		os.Exit(onCmd(args))
	case "off":
		os.Exit(offCmd(args))
	case "status":
		os.Exit(statusCmd(args))
	case "hook":
		os.Exit(hookCmd(args))
	case "context":
		os.Exit(contextCmd(args))
	case "agents":
		agents(args)
	case "doctor":
		os.Exit(doctorCmd(args))
	case "subagents":
		os.Exit(subagentsCmd(args))
	case "tiers":
		tiersCmd(args)
	case "config":
		os.Exit(configCmd(args))
	case "route":
		os.Exit(runCommand(args, true))
	case "classify":
		os.Exit(classifyCmd(args))
	case "run":
		os.Exit(run(args))
	case "result":
		os.Exit(result(args))
	case "wait":
		os.Exit(wait(args))
	case "stats":
		os.Exit(statsCmd(args))
	case "list":
		os.Exit(list(args))
	case "cancel":
		os.Exit(cancel(args))
	case "recover":
		os.Exit(recoverCmd(args))
	case "verify":
		os.Exit(verifyCmd(args))
	case "merge":
		os.Exit(merge(args))
	case "clean":
		os.Exit(clean(args))
	case "skill":
		os.Exit(skillCmd(args))
	case "version", "--version":
		fmt.Println(version)
	case "update":
		os.Exit(updateCmd(args))
	case "-h", "--help", "help":
		fmt.Println(usageText)
	default:
		fatalUsage("unknown command " + os.Args[1])
	}
}

func fatalUsage(msg string) {
	if msg != "" {
		fmt.Fprintln(os.Stderr, "dispatch:", msg)
	}
	fmt.Fprintln(os.Stderr, usageText)
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "dispatch:", sandbox.Redact(err.Error()))
	os.Exit(1)
}

func home() string {
	if h := os.Getenv("DISPATCH_HOME"); h != "" {
		return h
	}
	u, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	return filepath.Join(u, ".dispatch")
}

func tiersPath() string { return filepath.Join(home(), "tiers.json") }
func runsStore() store.Store {
	return store.Store{Path: filepath.Join(home(), "runs.jsonl")}
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func printCompact(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(b))
}

// emitSummary prints the RunSummary. JSON is the default; --json=false is three lines.
func emitSummary(rec store.Record, asJSON bool) {
	sum := runner.Summarize(rec)
	if asJSON {
		printCompact(sum)
		return
	}
	fmt.Print(sum.Text())
}

// --- agents ---

type quotaInfo struct {
	Source     string                `json:"source"`
	Confidence adapter.Confidence    `json:"confidence"`
	Risk       string                `json:"risk,omitempty"`
	Samples    []adapter.QuotaSample `json:"samples,omitempty"`
}

type agentInfo struct {
	Name         string               `json:"name"`
	Binary       string               `json:"binary"`
	Path         string               `json:"path,omitempty"`
	Installed    bool                 `json:"installed"`
	Capabilities adapter.Capabilities `json:"capabilities"`
	Models       []adapter.Model      `json:"models,omitempty"`
	Quota        quotaInfo            `json:"quota"`
}

func quotaFor(a adapter.Adapter, now time.Time) quotaInfo {
	if a.Capabilities().Usage {
		if u, ok := a.(adapter.UsageProvider); ok {
			return quotaInfo{Source: u.UsageSource(), Confidence: adapter.ConfidenceOfficial}
		}
	}
	s := runner.EmpiricalQuota(runsStore(), a.Name(), now)
	return quotaInfo{
		Source:     s.Source,
		Confidence: s.Confidence,
		Risk:       s.Risk,
		Samples:    []adapter.QuotaSample{s},
	}
}

func (q quotaInfo) human() string {
	s := fmt.Sprintf("quota: source=%s confidence=%s", q.Source, q.Confidence)
	if q.Risk != "" {
		s += " risk=" + q.Risk
	}
	if q.Confidence == adapter.ConfidenceEmpirical {
		s += " used=unknown"
	}
	if len(q.Samples) == 1 && !q.Samples[0].ResetAt.IsZero() {
		s += " resetAt=" + q.Samples[0].ResetAt.UTC().Format(time.RFC3339)
	}
	return s
}

func agents(args []string) {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)
	now := time.Now()
	var infos []agentInfo
	for _, a := range adapter.Known() {
		info := agentInfo{
			Name: a.Name(), Binary: a.Binary(), Capabilities: a.Capabilities(),
			Quota: quotaFor(a, now),
		}
		if p, err := exec.LookPath(a.Binary()); err == nil {
			info.Installed, info.Path = true, p
		}
		infos = append(infos, info)
	}
	if *asJSON {
		for i := range infos {
			if infos[i].Installed {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				infos[i].Models, _ = adapter.Known()[i].Models(ctx)
				cancel()
			}
		}
		printJSON(infos)
		return
	}
	for _, i := range infos {
		mark := "-"
		if i.Installed {
			mark = "+"
		}
		fmt.Printf("%s %-12s %s\n", mark, i.Name, i.Path)
		fmt.Printf("  %s\n", i.Quota.human())
	}
}

// doctorCmd prints probe results for installed CLIs. Exit 0 unless encoding fails.
func doctorCmd(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reports := adapter.Doctor(ctx)
	table, tableErr := tiers.Load(tiersPath())
	var tableProblems []string
	if tableErr != nil {
		tableProblems = []string{tableErr.Error()}
	} else {
		tableProblems = table.CapabilityProblems(adapter.Known())
	}
	for i := range reports {
		if len(tableProblems) > 0 {
			reports[i].Status = adapter.StatusDegraded
			reports[i].Notes = append(reports[i].Notes, tableProblems...)
		}
	}
	if len(reports) == 0 && len(tableProblems) > 0 {
		fmt.Fprintln(os.Stderr, strings.Join(tableProblems, "; "))
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			fmt.Fprintln(os.Stderr, "dispatch:", sandbox.Redact(err.Error()))
			return 1
		}
		return 0
	}
	if len(reports) == 0 {
		fmt.Println("no installed agent CLIs")
		return 0
	}
	for _, r := range reports {
		ver := r.Version
		if ver == "" {
			ver = "unknown"
		}
		fmt.Printf("%-12s %-14s %s\n", r.Name, ver, r.Status)
		var have, miss []string
		for _, f := range r.Flags {
			if f.Present {
				have = append(have, f.Name)
			} else {
				miss = append(miss, f.Name)
			}
		}
		if len(have) > 0 {
			fmt.Printf("  flags: %s\n", strings.Join(have, " "))
		}
		if len(miss) > 0 {
			fmt.Printf("  missing: %s\n", strings.Join(miss, " "))
		}
		fmt.Printf("  capabilities: %s\n", formatCaps(r.Capabilities))
		for _, n := range r.Notes {
			fmt.Printf("  note: %s\n", sandbox.Redact(n))
		}
	}
	return 0
}

func formatCaps(c adapter.Capabilities) string {
	var parts []string
	mode := c.EffortMode
	if mode == "" {
		mode = "unsupported"
	}
	parts = append(parts, "effort="+mode)
	if c.SessionResume {
		parts = append(parts, "sessionResume")
	}
	if c.StructuredOutput {
		parts = append(parts, "structuredOutput")
	}
	if c.ReadOnly {
		parts = append(parts, "readOnly")
	}
	if c.FileEditing {
		parts = append(parts, "fileEditing")
	}
	if c.Usage {
		parts = append(parts, "usage")
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

// --- tiers ---

func tiersCmd(args []string) {
	fs := flag.NewFlagSet("tiers", flag.ExitOnError)
	init := fs.Bool("init", false, "write the default table to tiers.json for editing")
	check := fs.Bool("check", false, "validate the table against installed agents and models")
	fs.Parse(args)
	if *init {
		if err := os.MkdirAll(home(), 0o755); err != nil {
			fatal(err)
		}
		if err := tiers.WriteDefault(tiersPath()); err != nil {
			fatal(err)
		}
		fmt.Println("wrote", tiersPath())
		return
	}
	t, err := tiers.Load(tiersPath())
	if err != nil {
		fatal(err)
	}
	if *check {
		os.Exit(checkTiers(t))
	}
	printJSON(t)
}

// checkTiers reports table entries whose agent is unknown, not installed,
// or whose model the agent does not list. Exit 1 when any problem exists.
func checkTiers(t tiers.Table) int {
	known := map[string]adapter.Adapter{}
	for _, a := range adapter.Known() {
		known[a.Name()] = a
	}
	models := map[string]map[string]bool{}
	problems := append(t.FieldProblems(), t.CapabilityProblems(adapter.Known())...)
	if p, err := config.LoadAll(config.Path(home()), filepath.Join(mustWD(), ".dispatch", "config.json")); err != nil {
		problems = append(problems, err.Error())
	} else {
		for tier, entries := range t.Tiers {
			allowed := false
			for _, e := range entries {
				provider := e.Provider
				if provider == "" {
					provider = adapter.ProviderOf(e.Agent, e.Model)
				}
				if config.PolicyReason(p, provider, e.Model, e.Agent) == "" {
					allowed = true
					break
				}
			}
			if !allowed && len(entries) > 0 {
				problems = append(problems, fmt.Sprintf("%s: policy eliminates entire tier (no candidate)", tier))
			}
		}
	}
	for tier, entries := range t.Tiers {
		for _, e := range entries {
			a, ok := known[e.Agent]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s: unknown agent %q", tier, e.Agent))
				continue
			}
			if _, err := exec.LookPath(a.Binary()); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %s is not installed", tier, e.Agent))
				continue
			}
			if models[e.Agent] == nil {
				models[e.Agent] = map[string]bool{}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				ms, err := a.Models(ctx)
				cancel()
				if err != nil {
					problems = append(problems, fmt.Sprintf("%s: cannot list models: %v", e.Agent, err))
				}
				for _, m := range ms {
					models[e.Agent][m.ID] = true
				}
			}
			if len(models[e.Agent]) > 0 && !models[e.Agent][e.Model] {
				problems = append(problems, fmt.Sprintf("%s: %s does not list model %q", tier, e.Agent, e.Model))
			}
			if a.Capabilities().EffortMode == "model-suffix" {
				for _, effort := range e.Efforts {
					variant := adapter.BaseModel(e.Model) + "-" + effort
					if len(models[e.Agent]) > 0 && !models[e.Agent][variant] {
						problems = append(problems, fmt.Sprintf("%s: %s does not list effort variant %q", tier, e.Agent, variant))
					}
				}
			}
		}
	}
	sort.Strings(problems)
	if len(problems) == 0 {
		fmt.Println("ok: every entry resolves to an installed agent and a listed model")
		return 0
	}
	for _, p := range problems {
		fmt.Println("!", p)
	}
	return 1
}

// --- run ---

// parseInterspersed parses flags that may appear before or after
// positional arguments and returns the positionals.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			return pos
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func deps() runner.Deps { return depsFor("") }

func depsFor(dir string) runner.Deps {
	table, err := tiers.Load(tiersPath())
	if err != nil {
		fatal(err)
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	policy, err := config.LoadAll(config.Path(home()), filepath.Join(dir, ".dispatch", "config.json"))
	if err != nil {
		fatal(err)
	}
	return runner.Deps{
		Adapters: adapter.Known(), Table: table, Store: runsStore(), Now: time.Now,
		Policy: policy,
	}
}

func configPath(project bool) string {
	if !project {
		return config.Path(home())
	}
	dir, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	return filepath.Join(dir, ".dispatch", "config.json")
}

func configCmd(args []string) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	project := fs.Bool("project", false, "read/write <repo>/.dispatch/config.json")
	var clean []string
	for _, v := range args {
		if v == "--project" {
			*project = true
		} else {
			clean = append(clean, v)
		}
	}
	if err := fs.Parse(clean); err != nil {
		return 2
	}
	a := fs.Args()
	if len(a) < 1 {
		fmt.Fprintln(os.Stderr, "usage: dispatch config get|set|allow|deny ...")
		return 2
	}
	path := configPath(*project)
	if a[0] == "get" {
		var f config.File
		var err error
		if *project {
			f, err = config.Load(path)
		} else {
			f, err = config.LoadAll(config.Path(home()), filepath.Join(mustWD(), ".dispatch", "config.json"))
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "dispatch:", err)
			return 1
		}
		if len(a) == 1 {
			printJSON(f)
			return 0
		}
		switch a[1] {
		case "priority":
			fmt.Println(f.Priority)
		case "discover":
			fmt.Println(f.Discover)
		case "allow":
			printJSON(f.Allow)
		case "deny":
			printJSON(f.Deny)
		default:
			fmt.Fprintln(os.Stderr, "dispatch: unknown config key", a[1])
			return 2
		}
		return 0
	}
	f, err := config.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dispatch:", err)
		return 1
	}
	switch a[0] {
	case "set":
		if len(a) != 3 {
			return configUsage()
		}
		switch a[1] {
		case "priority":
			f.Priority = a[2]
		case "discover":
			if a[2] != "true" && a[2] != "false" {
				fmt.Fprintln(os.Stderr, "dispatch: discover must be true or false")
				return 2
			}
			f.Discover = a[2] == "true"
		default:
			return configUsage()
		}
		if _, err := validateConfig(f); err != nil {
			fmt.Fprintln(os.Stderr, "dispatch:", err)
			return 1
		}
	case "allow", "deny":
		if len(a) < 4 || (a[1] != "add" && a[1] != "rm") {
			return configUsage()
		}
		r := &f.Allow
		if a[0] == "deny" {
			r = &f.Deny
		}
		var dst *[]string
		switch a[2] {
		case "providers":
			dst = &r.Providers
		case "models":
			dst = &r.Models
		case "agents":
			dst = &r.Agents
		default:
			return configUsage()
		}
		for _, v := range a[3:] {
			if a[1] == "add" {
				if !slices.Contains(*dst, v) {
					*dst = append(*dst, v)
				}
			} else {
				*dst = slices.DeleteFunc(*dst, func(x string) bool { return x == v })
			}
		}
	default:
		return configUsage()
	}
	key := a[0]
	var value any
	if key == "set" {
		key = a[1]
		if key == "priority" {
			value = f.Priority
		} else {
			value = f.Discover
		}
	} else if key == "allow" {
		value = f.Allow
	} else {
		value = f.Deny
	}
	if err := config.WriteKey(path, key, value); err != nil {
		fmt.Fprintln(os.Stderr, "dispatch:", err)
		return 1
	}
	return 0
}

func mustWD() string {
	d, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	return d
}
func validateConfig(f config.File) (config.File, error) { return f, config.Validate(f) }
func configUsage() int {
	fmt.Fprintln(os.Stderr, "usage: dispatch config set priority|discover <value> | config allow|deny add|rm providers|models|agents <value...>")
	return 2
}

func view(rec store.Record) map[string]any {
	sum, truncated := runner.Summary(rec.Output, 40, 4000)
	_, total, _ := runner.Page(rec.Output, 0, 0)
	v := map[string]any{
		"id": rec.ID, "status": rec.Status, "agent": rec.Agent, "model": rec.Model,
		"summary": sum, "truncated": truncated, "totalLines": total,
	}
	for k, val := range map[string]string{
		"error": rec.Error, "sessionId": rec.SessionID, "branch": rec.Branch,
		"worktree": rec.Worktree, "note": rec.Note, "resumedOf": rec.ResumedOf,
	} {
		if val != "" {
			v[k] = val
		}
	}
	if n := len(rec.Attempts); n > 0 {
		v["attempts"] = n
	}
	if vfy := compactVerification(rec); vfy != nil {
		v["verification"] = vfy
	}
	return v
}

// compactVerification is the run/wait/meta view: no tails.
func compactVerification(rec store.Record) map[string]any {
	if rec.Verification == "" && rec.Verify == nil {
		return nil
	}
	var passed, failed int
	var regs []string
	var warnings []string
	if rec.Verify != nil {
		warnings = append([]string{}, rec.Verify.Warnings...)
		for _, c := range rec.Verify.Checks {
			switch c.Status {
			case verify.CheckPassed:
				passed++
			case verify.CheckFailed, verify.CheckTimeout:
				failed++
			}
			regs = append(regs, c.Regressions...)
		}
	}
	if warnings == nil {
		warnings = []string{}
	}
	if regs == nil {
		regs = []string{}
	}
	if len(regs) > 10 {
		regs = regs[:10]
	}
	status := rec.Verification
	if status == "" && rec.Verify != nil {
		status = string(rec.Verify.Status)
	}
	return map[string]any{
		"status": status, "passed": passed, "failed": failed,
		"regressions": regs, "warnings": warnings,
	}
}

// metaView is the record without the agent output or verification tails.
func metaView(rec store.Record) any {
	full := rec.Verify
	rec.Output = ""
	rec.Verify = nil
	b, err := json.Marshal(rec)
	if err != nil {
		return rec
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return rec
	}
	delete(m, "verify")
	rec.Verify = full
	if v := compactVerification(rec); v != nil {
		m["verification"] = v
	} else {
		delete(m, "verification")
	}
	return m
}

func exitCode(s store.Status) int {
	switch s {
	case store.Completed:
		return 0
	case store.TimedOut:
		return 124
	default:
		return 1
	}
}

func readPrompt(words []string) string {
	prompt := strings.TrimSpace(strings.Join(words, " "))
	if prompt == "" || prompt == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal(err)
		}
		prompt = strings.TrimSpace(string(b))
	}
	return prompt
}

func run(args []string) int { return runCommand(args, false) }

func runCommand(args []string, dry bool) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	tier := fs.String("tier", "", "easy|normal|hard")
	dir := fs.String("dir", "", "working directory for the agent")
	ro := fs.Bool("readonly", false, "forbid file edits")
	timeout := fs.Duration("timeout", 0, "max run time (0 = none)")
	bg := fs.Bool("background", false, "return the id immediately; use wait/result")
	noWT := fs.Bool("no-worktree", false, "write directly in --dir instead of a git worktree")
	noVerify := fs.Bool("no-verify", false, "skip deterministic checks after a write run")
	resume := fs.String("resume", "", "continue the session of an earlier run id")
	as := fs.String("as", "", "subagent profile from agents.json (explicit flags override it)")
	execID := fs.String("exec", "", "internal: execute a pre-created run")
	asJSON := fs.Bool("json", true, "compact RunSummary JSON; --json=false prints 3 lines")
	provider := fs.String("provider", "", "restrict model provider")
	model := fs.String("model", "", "restrict model")
	effort := fs.String("effort", "", "low|medium|high|xhigh")
	agent := fs.String("agent", "", "restrict CLI agent")
	relax := fs.Bool("relax", false, "allow fallback to tier routing")
	priority := fs.String("priority", "", "speed|balanced|quality (default balanced)")
	words := parseInterspersed(fs, args)
	if dry {
		if *execID != "" || *resume != "" || *bg {
			fmt.Fprintln(os.Stderr, "route does not support --exec, --resume or --background")
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := deps()

	if !dry && *execID != "" { // detached child of --background
		rec, err := d.Store.Get(*execID)
		if err != nil {
			fatal(err)
		}
		rec, err = d.Execute(ctx, rec)
		if err != nil {
			fatal(err)
		}
		printCompact(runner.Summarize(rec))
		return exitCode(rec.Status)
	}

	if !dry {
		if err := d.Reconcile(ctx); err != nil {
			fatal(err)
		}
	}
	prompt := ""
	if !dry || len(words) > 0 {
		prompt = readPrompt(words)
	}
	if prompt == "" && !dry {
		fatalUsage("empty prompt")
	}
	opts := runner.Options{
		Filters: store.Filters{Provider: *provider, Model: *model, Effort: *effort, Agent: *agent, Relax: *relax, Priority: *priority},
		Tier:    adapter.Tier(*tier), Prompt: prompt, Dir: *dir, ReadOnly: *ro,
		Timeout: *timeout, NoWorktree: *noWT, NoVerify: *noVerify, Resume: *resume,
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *as != "" {
		if err := applyProfile(&opts, *as, set); err != nil {
			fatal(err)
		}
	}
	projectConfig := filepath.Join(mustWD(), ".dispatch", "config.json")
	if opts.Dir != "" {
		projectConfig = filepath.Join(opts.Dir, ".dispatch", "config.json")
	}
	resolvedPriority, err := resolveRoutingPriority(home(), opts.Filters.Priority, set["priority"], projectConfig)
	if err != nil {
		fatal(err)
	}
	opts.Filters.Priority = resolvedPriority
	policy, err := config.LoadAll(config.Path(home()), projectConfig)
	if err != nil {
		fatal(err)
	}
	d.Policy = policy

	if opts.Tier == "" && opts.Filters.Model == "" && opts.Filters.Provider == "" {
		opts.Tier = adapter.TierNormal
	}
	if dry {
		report, err := d.Route(ctx, opts)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		jsonSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "json" {
				jsonSet = true
			}
		})
		if jsonSet && *asJSON {
			printJSON(report)
		} else {
			for _, c := range report.Candidates {
				fmt.Printf("candidate: %s/%s provider=%s effort=%s %s\n", c.Agent, c.Model, c.Provider, c.Effort, c.Reason)
			}
			for _, reason := range report.Eliminated {
				fmt.Println("eliminated:", reason)
			}
			if report.Winner != nil {
				fmt.Printf("winner: %s/%s: %s\n", report.Winner.Agent, report.Winner.Model, report.Reason)
			} else {
				fmt.Println("FAILED:", report.Reason)
			}
			if report.Note != "" {
				fmt.Println("note:", report.Note)
			}
			if report.PriorityNote != "" {
				fmt.Println("priority:", report.PriorityNote)
			}
		}
		if report.Winner == nil {
			return 1
		}
		return 0
	}
	if *bg {
		rec, err := d.Begin(opts)
		if err != nil {
			fatal(err)
		}
		if err := spawnDetached(rec.ID); err != nil {
			fatal(err)
		}
		printJSON(map[string]any{"id": rec.ID, "status": rec.Status, "background": true})
		return 0
	}

	rec, err := d.Run(ctx, opts)
	if err != nil {
		fatal(err)
	}
	emitSummary(rec, *asJSON)
	return exitCode(rec.Status)
}

// spawnDetached starts `dispatch run --exec <id>` in its own session with
// output going to a per-run log, so it outlives the caller.
func spawnDetached(id string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logDir := filepath.Join(home(), "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(logDir, id+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "run", "--exec", id)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Process.Release()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, err := runsStore().Get(id)
		if err != nil {
			return err
		}
		if rec.PID == cmd.Process.Pid || rec.Status != store.Pending {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("background runner did not claim run %s", id)
}

// --- wait / list / cancel ---

func wait(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	timeout := fs.Duration("timeout", 0, "give up after this long (0 = no limit)")
	asJSON := fs.Bool("json", true, "compact RunSummary JSON; --json=false prints 3 lines")
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 {
		fatalUsage("wait needs one run id")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	rec, err := deps().Wait(ctx, pos[0], 500*time.Millisecond)
	if err != nil && rec.ID == "" {
		fatal(err)
	}
	emitSummary(rec, *asJSON)
	if err != nil { // still running when we stopped waiting
		return 3
	}
	return exitCode(rec.Status)
}

func statsCmd(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	by := fs.String("by", "model", "model|agent|provider|tier|effort|day|week|requested|resolved")
	since := fs.String("since", "7d", "window such as 7d, 24h, 30m, 2w, or all")
	asJSON := fs.Bool("json", false, "print JSON")
	capacity := fs.Bool("capacity", false, "quota windows from snapshots.jsonl")
	fs.Parse(args)
	now := time.Now()
	start, err := stats.ParseSince(*since, now)
	if err != nil {
		fatal(err)
	}
	if *capacity {
		rep, err := stats.CapacityFromStore(runsStore(), start, now)
		if err != nil {
			fatal(err)
		}
		if *asJSON {
			printCompact(rep)
		} else {
			fmt.Print(rep.Text())
		}
		return 0
	}
	rep, err := stats.FromStore(runsStore(), stats.Options{By: *by, Since: start, Now: now})
	if err != nil {
		fatal(err)
	}
	if *asJSON {
		printCompact(rep)
	} else {
		fmt.Print(rep.Text())
	}
	return 0
}

func list(args []string) int {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	status := fs.String("status", "", "filter: PENDING|RUNNING|ORPHANED|COMPLETED|FAILED|CANCELLED|TIMED_OUT")
	limit := fs.Int("limit", 20, "max rows (0 = all)")
	fs.Parse(args)
	if err := deps().Reconcile(context.Background()); err != nil {
		fatal(err)
	}
	recs, err := runsStore().List()
	if err != nil {
		fatal(err)
	}
	rows := []map[string]any{}
	for _, r := range recs {
		if *status != "" && !strings.EqualFold(string(r.Status), *status) {
			continue
		}
		p := r.Prompt
		if len(p) > 80 {
			p = p[:80] + "…"
		}
		rows = append(rows, map[string]any{
			"id": r.ID, "status": r.Status, "tier": r.Tier, "agent": r.Agent, "model": r.Model,
			"createdAt": r.CreatedAt, "prompt": strings.ReplaceAll(p, "\n", " "),
		})
		if *limit > 0 && len(rows) >= *limit {
			break
		}
	}
	printJSON(rows)
	return 0
}

func cancel(args []string) int {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 {
		fatalUsage("cancel needs one run id")
	}
	rec, err := deps().Cancel(context.Background(), pos[0])
	if err != nil {
		fatal(err)
	}
	printJSON(view(rec))
	return 0
}

// --- recover / merge / clean ---

func recoverCmd(args []string) int {
	fs := flag.NewFlagSet("recover", flag.ExitOnError)
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 {
		fatalUsage("recover needs one run id")
	}
	rec, stat, err := deps().Recover(context.Background(), pos[0])
	if err != nil {
		fatal(err)
	}
	out := view(rec)
	out["diffStat"] = stat
	printJSON(out)
	return 0
}

func merge(args []string) int {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	force := fs.Bool("force", false, "merge even when verification failed")
	noVerify := fs.Bool("no-verify", false, "skip post-merge verification")
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 {
		fatalUsage("merge needs one run id")
	}
	rec, err := runsStore().Get(pos[0])
	if err != nil {
		fatal(err)
	}
	if rec.Branch == "" || rec.Repo == "" {
		fatal(fmt.Errorf("run %s has no worktree branch to merge", rec.ID))
	}
	if rec.Status != store.Completed {
		fatal(fmt.Errorf("run %s is %s; only COMPLETED runs can be merged", rec.ID, rec.Status))
	}
	if err := mergeRefusal(rec, *force); err != nil {
		fatal(err)
	}

	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "dispatch:", sandbox.Redact(err.Error()))
		return 1
	}

	targetBranch, _ := worktree.CurrentBranch(rec.Repo)
	unlock, err := worktree.LockMerge(rec.Repo, targetBranch)
	if err != nil {
		return fail(err)
	}
	defer unlock()

	if overlapping, err := worktree.UncommittedOverlaps(rec.Repo, rec.Branch); err != nil {
		return fail(err)
	} else if len(overlapping) > 0 {
		return fail(fmt.Errorf("working tree has uncommitted changes in files touched by %s: %s; commit or stash them before merging", rec.Branch, strings.Join(overlapping, ", ")))
	}

	if worktree.SupportsMergeTree(rec.Repo) {
		res, err := worktree.MergeTree(rec.Repo, "HEAD", rec.Branch)
		if err != nil {
			return fail(err)
		}
		if !res.Clean {
			rec.ExitClass = "merge_conflict"
			_ = runsStore().Append(rec)

			body := map[string]any{
				"id":        rec.ID,
				"status":    "merge_conflict",
				"conflicts": res.Conflicts,
				"hint":      fmt.Sprintf("dispatch run --resume %s (pedindo rebase), ou resolver a mão", rec.ID),
			}
			printJSON(body)
			return 1
		}
	} else {
		fmt.Fprintln(os.Stderr, "dispatch: warning: git version does not support merge-tree --write-tree; skipping pre-merge conflict check")
	}

	headBeforeMerge, _ := worktree.Head(rec.Repo)
	out, err := worktree.Merge(rec.Repo, rec.Branch, rec.ID)
	if err != nil {
		return fail(err)
	}
	mergeSHA, _ := worktree.Head(rec.Repo)

	cleaned := true
	if err := worktree.Remove(rec.Repo, rec.Worktree, rec.Branch, false); err != nil {
		cleaned = false
	}
	body := map[string]any{"id": rec.ID, "merged": rec.Branch, "git": out, "cleaned": cleaned}
	if note := mergeNote(rec, *force); note != "" {
		body["note"] = note
	}

	postVerifyFailed := false
	if !*noVerify && verify.HasConfig(rec.Repo) {
		vRes, vErr := verify.Run(context.Background(), verify.Options{
			Repo:     rec.Repo,
			Worktree: rec.Repo,
			BaseSHA:  headBeforeMerge,
		})
		if vErr != nil || vRes.Status == verify.StatusFailed || vRes.Status == verify.StatusError {
			postVerifyFailed = true
			body["post_merge_verification"] = "failed"
			revertCmd := fmt.Sprintf("git revert -m 1 %s", mergeSHA)
			body["revert"] = revertCmd
			if len(vRes.Checks) > 0 {
				body["verify"] = vRes
			}
		} else if vRes.Status == verify.StatusVerified {
			body["post_merge_verification"] = "passed"
		}
	}

	printJSON(body)
	if postVerifyFailed {
		return 1
	}
	return 0
}

func mergeRefusal(rec store.Record, force bool) error {
	if force || rec.Verification != string(verify.StatusFailed) {
		return nil
	}
	regs := regressionLines(rec, 20)
	msg := fmt.Sprintf("run %s failed verification", rec.ID)
	if len(regs) > 0 {
		msg += ": " + strings.Join(regs, ", ")
	}
	return fmt.Errorf("%s; use --force to merge anyway", msg)
}

func mergeNote(rec store.Record, forced bool) string {
	if rec.Verification == string(verify.StatusFailed) && forced {
		regs := regressionLines(rec, 20)
		if len(regs) == 0 {
			return "merged despite verification_failed"
		}
		return "merged despite verification_failed: " + strings.Join(regs, ", ")
	}
	switch rec.Verification {
	case "":
		return "run was not verified"
	case string(verify.StatusUnverified):
		return "verification skipped; nothing to verify"
	case string(verify.StatusError):
		return "verifier error; merge was not blocked"
	default:
		return ""
	}
}

func regressionLines(rec store.Record, limit int) []string {
	if rec.Verify == nil {
		return nil
	}
	var out []string
	for _, c := range rec.Verify.Checks {
		out = append(out, c.Regressions...)
		if limit > 0 && len(out) >= limit {
			return out[:limit]
		}
	}
	return out
}

func verifyCmd(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	init := fs.Bool("init", false, "write an example .dispatch/verify.json in the current repo")
	pos := parseInterspersed(fs, args)
	if *init {
		if len(pos) > 0 {
			fatalUsage("verify --init does not take a run id")
		}
		dir, err := os.Getwd()
		if err != nil {
			fatal(err)
		}
		if root := worktree.Root(dir); root != "" {
			dir = root
		}
		path := verify.ConfigPath(dir)
		if _, err := os.Stat(path); err == nil {
			fatal(fmt.Errorf("%s already exists", path))
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(path, []byte(verify.Example), 0o644); err != nil {
			fatal(err)
		}
		fmt.Println("wrote", path)
		return 0
	}
	if len(pos) != 1 {
		fatalUsage("verify needs one run id")
	}
	rec, err := runsStore().Get(pos[0])
	if err != nil {
		fatal(err)
	}
	if rec.Worktree == "" {
		fatal(fmt.Errorf("run %s has no worktree", rec.ID))
	}
	if info, err := os.Stat(rec.Worktree); err != nil || !info.IsDir() {
		fatal(fmt.Errorf("worktree %s does not exist", rec.Worktree))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := verify.Run(ctx, verify.Options{
		Repo: rec.Repo, Worktree: rec.Worktree, BaseSHA: rec.BaseSHA,
	})
	rec.Verify = &res
	rec.Verification = string(res.Status)
	rec.VerifyMs = res.DurationMs
	if rec.Status == store.Completed || rec.Status == store.Failed || rec.Status == store.TimedOut || rec.Status == store.Cancelled {
		rec.ExitClass = runner.Classify(rec)
	}
	if err != nil {
		rec.Note = joinNotes(rec.Note, "verification: "+err.Error())
	}
	if err := runsStore().Append(rec); err != nil {
		fatal(err)
	}
	printJSON(map[string]any{
		"id": rec.ID, "status": rec.Status, "exitClass": rec.ExitClass,
		"verification": compactVerification(rec), "result": res,
	})
	if res.Status == verify.StatusFailed || res.Status == verify.StatusError {
		return 1
	}
	return 0
}

func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

func clean(args []string) int {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	force := fs.Bool("force", false, "discard even if the branch is not merged")
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 {
		fatalUsage("clean needs one run id")
	}
	rec, err := runsStore().Get(pos[0])
	if err != nil {
		fatal(err)
	}
	if rec.Worktree == "" {
		fatal(fmt.Errorf("run %s has no worktree", rec.ID))
	}
	if err := worktree.Remove(rec.Repo, rec.Worktree, rec.Branch, *force); err != nil {
		fatal(err)
	}
	printJSON(map[string]any{"id": rec.ID, "removed": rec.Worktree})
	return 0
}

// --- result ---

func result(args []string) int {
	if err := deps().Reconcile(context.Background()); err != nil {
		fatal(err)
	}
	fs := flag.NewFlagSet("result", flag.ExitOnError)
	section := fs.String("section", "summary", "summary|verification|files|agent|attempts|meta|output")
	offset := fs.Int("offset", 0, "first line of agent or verification text")
	limit := fs.Int("limit", 200, "max lines (0 = all)")
	asJSON := fs.Bool("json", true, "with --section summary, --json=false prints 3 lines")
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 {
		fatalUsage("result needs one run id")
	}
	rec, err := runsStore().Get(pos[0])
	if err != nil {
		fatal(fmt.Errorf("%s: %w", pos[0], err))
	}
	switch *section {
	case "summary":
		if *asJSON {
			printJSON(runner.Summarize(rec))
		} else {
			fmt.Print(runner.Summarize(rec).Text())
		}
	case "agent", "output":
		content, total, more := runner.Page(rec.Output, *offset, *limit)
		printJSON(map[string]any{
			"id": rec.ID, "status": rec.Status, "section": *section,
			"offset": *offset, "limit": *limit, "totalLines": total, "hasMore": more, "content": content,
		})
	case "files":
		printJSON(filesView(rec))
	case "attempts":
		printJSON(map[string]any{"id": rec.ID, "status": rec.Status, "attempts": rec.Attempts})
	case "meta":
		printJSON(metaView(rec))
	case "verification":
		payload := rec.Verify
		if payload == nil {
			payload = &verify.Result{Status: verify.Status(rec.Verification)}
		}
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			fatal(err)
		}
		content, total, more := runner.Page(string(b)+"\n", *offset, *limit)
		printJSON(map[string]any{
			"id": rec.ID, "status": rec.Status, "section": "verification",
			"offset": *offset, "limit": *limit, "totalLines": total, "hasMore": more, "content": content,
		})
	default:
		fatalUsage("unknown section " + *section)
	}
	return 0
}

func filesView(rec store.Record) map[string]any {
	m := map[string]any{"id": rec.ID, "status": rec.Status, "section": "files"}
	var unknown []string
	if rec.FilesChanged == nil {
		unknown = append(unknown, "filesChanged")
	} else {
		m["filesChanged"] = *rec.FilesChanged
	}
	if rec.Insertions == nil {
		unknown = append(unknown, "insertions")
	} else {
		m["insertions"] = *rec.Insertions
	}
	if rec.Deletions == nil {
		unknown = append(unknown, "deletions")
	} else {
		m["deletions"] = *rec.Deletions
	}
	if len(unknown) > 0 {
		m["unknown"] = unknown
		m["availability"] = "desconhecido"
	}
	return m
}

// --- skill ---

func skillCmd(args []string) int {
	if len(args) == 0 {
		fatalUsage("skill needs a subcommand: install | show | refresh")
	}
	switch args[0] {
	case "show":
		os.Stdout.Write(skill.Content)
		return 0
	case "install":
		return skillInstall(args[1:])
	case "refresh":
		u, err := os.UserHomeDir()
		if err != nil {
			fatal(err)
		}
		paths, err := skill.Refresh(u)
		for _, p := range paths {
			fmt.Printf("~ %s\n", p)
		}
		if err != nil {
			fatal(err)
		}
		return 0
	}
	fatalUsage("unknown skill subcommand " + args[0])
	return 2
}

func skillInstall(args []string) int {
	fs := flag.NewFlagSet("skill install", flag.ExitOnError)
	agentsFlag := fs.String("agent", "all", "comma-separated agents, or all (= installed ones)")
	force := fs.Bool("force", false, "overwrite an existing dispatch skill")
	fs.Parse(args)

	u, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	targets := skill.Targets(u)
	binaries := map[string]string{"claude": "claude"}
	for _, a := range adapter.Known() {
		binaries[a.Name()] = a.Binary()
	}

	var want []string
	if *agentsFlag == "all" {
		for name, bin := range binaries {
			if _, err := exec.LookPath(bin); err == nil {
				want = append(want, name)
			}
		}
	} else {
		want = strings.Split(*agentsFlag, ",")
	}
	sort.Strings(want)

	code, done := 0, map[string]bool{}
	for _, name := range want {
		t, ok := targets[strings.TrimSpace(name)]
		if !ok {
			fmt.Fprintf(os.Stderr, "dispatch: unknown agent %q\n", name)
			code = 2
			continue
		}
		if done[t.Dir] {
			fmt.Printf("= %-12s shares %s\n", name, t.Dir)
			continue
		}
		done[t.Dir] = true
		path, err := skill.Install(t.Dir, *force)
		if err != nil {
			fmt.Fprintf(os.Stderr, "! %-12s %v\n", name, err)
			code = 1
			continue
		}
		fmt.Printf("+ %-12s %s\n", name, path)
	}
	return code
}

// --- update ---

func updateCmd(args []string) int {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only report whether a newer release exists")
	fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	src := update.Source{Repo: os.Getenv("DISPATCH_REPO"), BaseURL: os.Getenv("DISPATCH_BASE_URL")}
	tag, err := src.Latest(ctx)
	if err != nil {
		fatal(err)
	}
	if tag == version {
		fmt.Printf("dispatch %s is up to date\n", version)
		return 0
	}
	if *check {
		fmt.Printf("update available: %s -> %s (run: dispatch update)\n", version, tag)
		return 0
	}
	self, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		fatal(err)
	}
	bin, err := src.Fetch(ctx, tag, update.AssetName())
	if err != nil {
		fatal(err)
	}
	if err := update.Replace(self, bin); err != nil {
		fatal(err)
	}
	fmt.Printf("updated %s -> %s (%s)\n", version, tag, self)
	// The new binary carries the new skill text; let it refresh the copies.
	cmd := exec.Command(self, "skill", "refresh")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "dispatch: skill refresh failed:", err)
		return 1
	}
	return 0
}

// --- subagents ---

// resolveRoutingPriority applies an explicit flag, else the current value
// (usually a subagent profile), else ~/.dispatch/config.json, else balanced.
func resolveRoutingPriority(dispatchHome, current string, explicit bool, project ...string) (string, error) {
	p := current
	if !explicit && p == "" {
		projectPath := ""
		if len(project) > 0 {
			projectPath = project[0]
		}
		cfg, err := config.LoadAll(config.Path(dispatchHome), projectPath)
		if err != nil {
			return "", err
		}
		p = cfg.Priority
	}
	if p == "" {
		p = router.PriorityBalanced
	}
	if !router.ValidPriority(p) {
		return "", fmt.Errorf("invalid priority %q (want speed|balanced|quality)", p)
	}
	return p, nil
}

func subagentPaths(dir string) (user, project string) {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return filepath.Join(home(), "agents.json"), subagent.ProjectPath(dir)
}

// applyProfile fills opts from the named profile. Flags the caller set
// explicitly (names in set) win over the profile.
func applyProfile(o *runner.Options, name string, set map[string]bool) error {
	user, project := subagentPaths(o.Dir)
	profiles, err := subagent.LoadAll(user, project)
	if err != nil {
		return err
	}
	p, ok := profiles[name]
	if !ok {
		return fmt.Errorf("unknown subagent %q (have: %s)", name, strings.Join(subagent.Names(profiles), ", "))
	}
	if !set["provider"] {
		o.Filters.Provider = p.Provider
	}
	if !set["model"] {
		o.Filters.Model = p.Model
	}
	if !set["effort"] {
		o.Filters.Effort = p.Effort
	}
	if !set["agent"] {
		o.Filters.Agent = p.Agent
	}
	if !set["relax"] {
		o.Filters.Relax = p.Relax
	}
	if !set["priority"] && p.Priority != "" {
		o.Filters.Priority = p.Priority
	}
	if !set["tier"] && p.Tier != "" {
		o.Tier = adapter.Tier(p.Tier)
	}
	if !set["readonly"] {
		o.ReadOnly = p.ReadOnly
	}
	if !set["timeout"] && p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil {
			return fmt.Errorf("subagent %s: invalid timeout %q", name, p.Timeout)
		}
		o.Timeout = d
	}
	if !set["no-verify"] {
		o.NoVerify = p.NoVerify
	}
	if p.Instructions != "" && o.Resume == "" {
		o.Prompt = p.Instructions + "\n\n" + o.Prompt
	}
	o.Profile, o.Prefer, o.Avoid = name, p.Prefer, p.Avoid
	return nil
}

func subagentsCmd(args []string) int {
	fs := flag.NewFlagSet("subagents", flag.ExitOnError)
	init := fs.Bool("init", false, "write an example agents.json to edit")
	check := fs.Bool("check", false, "validate profiles against known and installed agents")
	dir := fs.String("dir", "", "project directory (default: cwd)")
	fs.Parse(args)
	user, project := subagentPaths(*dir)
	if *init {
		if _, err := os.Stat(user); err == nil {
			fatal(fmt.Errorf("%s already exists", user))
		}
		if err := os.MkdirAll(home(), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(user, []byte(subagent.Example), 0o644); err != nil {
			fatal(err)
		}
		fmt.Println("wrote", user)
		return 0
	}
	profiles, err := subagent.LoadAll(user, project)
	if err != nil {
		fatal(err)
	}
	if !*check {
		printJSON(profiles)
		return 0
	}
	known := map[string]bool{}
	for _, a := range adapter.Known() {
		known[a.Name()] = true
	}
	t, err := tiers.Load(tiersPath())
	if err != nil {
		fatal(err)
	}
	var problems []string
	for _, name := range subagent.Names(profiles) {
		p := profiles[name]
		problems = append(problems, subagent.Validate(name, p, known)...)
		if p.Tier != "" && len(t.Tiers[adapter.Tier(p.Tier)]) == 0 {
			problems = append(problems, fmt.Sprintf("%s: tier %q has no entries in the tier table", name, p.Tier))
		}
		for _, a := range append(append([]string{}, p.Prefer...), p.Avoid...) {
			for _, k := range adapter.Known() {
				if k.Name() != a {
					continue
				}
				if _, err := exec.LookPath(k.Binary()); err != nil {
					problems = append(problems, fmt.Sprintf("%s: %s is not installed", name, a))
				}
			}
		}
	}
	if len(problems) == 0 {
		fmt.Printf("ok: %d subagent(s)\n", len(profiles))
		return 0
	}
	for _, p := range problems {
		fmt.Println("!", p)
	}
	return 1
}

// --- on / off / status / hook / context ---

func onCmd(args []string) int {
	u, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	if err := hook.SetEnabled(home(), true); err != nil {
		fatal(err)
	}
	if err := installMissingSkills(u); err != nil {
		fatal(err)
	}
	settingsPath := hook.SettingsPath(u)
	installed, err := hook.IsInstalled(settingsPath)
	if err != nil {
		fatal(err)
	}
	if !installed {
		execPath, err := os.Executable()
		if err != nil {
			fatal(err)
		}
		abs, err := filepath.Abs(execPath)
		if err != nil {
			fatal(err)
		}
		if err := hook.Install(settingsPath, abs); err != nil {
			fatal(err)
		}
		fmt.Printf("+ hook         %s\n", settingsPath)
	}
	fmt.Println("dispatch: on (ligado)")
	return 0
}

func offCmd(args []string) int {
	if err := hook.SetEnabled(home(), false); err != nil {
		fatal(err)
	}
	fmt.Println("dispatch: off (desligado)")
	return 0
}

func statusCmd(args []string) int {
	u, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	enabled := hook.IsEnabled(home())
	settingsPath := hook.SettingsPath(u)
	hookInstalled, _ := hook.IsInstalled(settingsPath)
	claudeDir := skill.Targets(u)["claude"].Dir
	skillInstalled := skill.IsInstalled(claudeDir)

	stateStr := "off (desligado)"
	if enabled {
		stateStr = "on (ligado)"
	}
	hookStr := "not installed"
	if hookInstalled {
		hookStr = "installed"
	}
	skillStr := "not installed"
	if skillInstalled {
		skillStr = "installed"
	}

	fmt.Printf("dispatch: %s\n", stateStr)
	fmt.Printf("hook:     %s\n", hookStr)
	fmt.Printf("skill:    %s\n", skillStr)
	return 0
}

func hookCmd(args []string) int {
	if len(args) == 0 {
		fatalUsage("hook needs a subcommand: install | uninstall")
	}
	u, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	settingsPath := hook.SettingsPath(u)
	switch args[0] {
	case "install":
		execPath, err := os.Executable()
		if err != nil {
			fatal(err)
		}
		abs, err := filepath.Abs(execPath)
		if err != nil {
			fatal(err)
		}
		if err := hook.Install(settingsPath, abs); err != nil {
			fatal(err)
		}
		fmt.Printf("+ hook %s\n", settingsPath)
		return 0
	case "uninstall":
		if err := hook.Uninstall(settingsPath); err != nil {
			fatal(err)
		}
		fmt.Printf("- hook %s\n", settingsPath)
		return 0
	default:
		fatalUsage("unknown hook subcommand " + args[0])
		return 2
	}
}

func contextCmd(args []string) int {
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	hookFlag := fs.Bool("hook", false, "context for session hook")
	fs.Parse(args)
	if !*hookFlag {
		fatalUsage("context requires --hook")
	}
	if !hook.IsEnabled(home()) {
		return 0
	}
	fmt.Print(hook.ContextBlock())
	return 0
}

func installMissingSkills(u string) error {
	targets := skill.Targets(u)
	binaries := map[string]string{"claude": "claude"}
	for _, a := range adapter.Known() {
		binaries[a.Name()] = a.Binary()
	}
	var want []string
	for name, bin := range binaries {
		if _, err := exec.LookPath(bin); err == nil {
			want = append(want, name)
		}
	}
	hasClaude := false
	for _, w := range want {
		if w == "claude" {
			hasClaude = true
			break
		}
	}
	if !hasClaude {
		want = append(want, "claude")
	}
	sort.Strings(want)

	done := map[string]bool{}
	for _, name := range want {
		t, ok := targets[name]
		if !ok || done[t.Dir] {
			continue
		}
		done[t.Dir] = true
		if !skill.IsInstalled(t.Dir) {
			path, err := skill.Install(t.Dir, false)
			if err != nil {
				return err
			}
			fmt.Printf("+ %-12s %s\n", name, path)
		}
	}
	return nil
}
