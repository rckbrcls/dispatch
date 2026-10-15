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
	"sort"
	"strings"
	"syscall"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/runner"
	"dispatch/internal/sandbox"
	"dispatch/internal/skill"
	"dispatch/internal/store"
	"dispatch/internal/subagent"
	"dispatch/internal/tiers"
	"dispatch/internal/update"
	"dispatch/internal/worktree"
)

// version is set at release time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const usageText = `usage:
  dispatch version
  dispatch update [--check]
  dispatch agents [--json]
  dispatch tiers [--init | --check]
  dispatch run --tier easy|normal|hard [--dir D] [--readonly] [--timeout 10m]
               [--background] [--no-worktree] <prompt | ->
  dispatch run --as <subagent> [flags] <prompt | ->   (profile from agents.json)
  dispatch subagents [--init | --check]
  dispatch run --resume <id> [--background] <follow-up prompt | ->
  dispatch wait <id> [--timeout 10m]
  dispatch list [--status S] [--limit N]
  dispatch cancel <id>
  dispatch merge <id>
  dispatch clean <id> [--force]
  dispatch skill install [--agent claude,codex,cursor,grok,antigravity|all] [--force]
  dispatch skill show
  dispatch skill refresh   (rewrite already-installed skill copies)
  dispatch result <id> [--section output|meta|attempts] [--offset N] [--limit N]

state lives in $DISPATCH_HOME (default ~/.dispatch): runs.jsonl, tiers.json, agents.json`

func main() {
	if len(os.Args) < 2 {
		fatalUsage("")
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "agents":
		agents(args)
	case "subagents":
		os.Exit(subagentsCmd(args))
	case "tiers":
		tiersCmd(args)
	case "run":
		os.Exit(run(args))
	case "result":
		os.Exit(result(args))
	case "wait":
		os.Exit(wait(args))
	case "list":
		os.Exit(list(args))
	case "cancel":
		os.Exit(cancel(args))
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

// --- agents ---

type agentInfo struct {
	Name         string               `json:"name"`
	Binary       string               `json:"binary"`
	Path         string               `json:"path,omitempty"`
	Installed    bool                 `json:"installed"`
	Capabilities adapter.Capabilities `json:"capabilities"`
	Models       []adapter.Model      `json:"models,omitempty"`
}

func agents(args []string) {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)
	var infos []agentInfo
	for _, a := range adapter.Known() {
		info := agentInfo{Name: a.Name(), Binary: a.Binary(), Capabilities: a.Capabilities()}
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
	}
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
	var problems []string
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

func deps() runner.Deps {
	table, err := tiers.Load(tiersPath())
	if err != nil {
		fatal(err)
	}
	return runner.Deps{
		Adapters: adapter.Known(), Table: table, Store: runsStore(), Now: time.Now,
	}
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
	return v
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

func run(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	tier := fs.String("tier", "normal", "easy|normal|hard")
	dir := fs.String("dir", "", "working directory for the agent")
	ro := fs.Bool("readonly", false, "forbid file edits")
	timeout := fs.Duration("timeout", 0, "max run time (0 = none)")
	bg := fs.Bool("background", false, "return the id immediately; use wait/result")
	noWT := fs.Bool("no-worktree", false, "write directly in --dir instead of a git worktree")
	resume := fs.String("resume", "", "continue the session of an earlier run id")
	as := fs.String("as", "", "subagent profile from agents.json (explicit flags override it)")
	execID := fs.String("exec", "", "internal: execute a pre-created run")
	words := parseInterspersed(fs, args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := deps()

	if *execID != "" { // detached child of --background
		rec, err := d.Store.Get(*execID)
		if err != nil {
			fatal(err)
		}
		rec, err = d.Execute(ctx, rec)
		if err != nil {
			fatal(err)
		}
		if rec.Output != "" {
			fmt.Fprintln(os.Stdout, sandbox.Redact(rec.Output))
		}
		return exitCode(rec.Status)
	}

	prompt := readPrompt(words)
	if prompt == "" {
		fatalUsage("empty prompt")
	}
	opts := runner.Options{
		Tier: adapter.Tier(*tier), Prompt: prompt, Dir: *dir, ReadOnly: *ro,
		Timeout: *timeout, NoWorktree: *noWT, Resume: *resume,
	}
	if *as != "" {
		set := map[string]bool{}
		fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if err := applyProfile(&opts, *as, set); err != nil {
			fatal(err)
		}
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
	if err != nil && rec.ID == "" {
		fatal(err)
	}
	printJSON(view(rec))
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
	return cmd.Process.Release()
}

// --- wait / list / cancel ---

func wait(args []string) int {
	fs := flag.NewFlagSet("wait", flag.ExitOnError)
	timeout := fs.Duration("timeout", 0, "give up after this long (0 = no limit)")
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
	printJSON(view(rec))
	if err != nil { // still running when we stopped waiting
		return 3
	}
	return exitCode(rec.Status)
}

func list(args []string) int {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	status := fs.String("status", "", "filter: PENDING|RUNNING|COMPLETED|FAILED|CANCELLED|TIMED_OUT")
	limit := fs.Int("limit", 20, "max rows (0 = all)")
	fs.Parse(args)
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

// --- merge / clean ---

func merge(args []string) int {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
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
	out, err := worktree.Merge(rec.Repo, rec.Branch, rec.ID)
	if err != nil {
		fatal(err)
	}
	cleaned := true
	if err := worktree.Remove(rec.Repo, rec.Worktree, rec.Branch, false); err != nil {
		cleaned = false
	}
	printJSON(map[string]any{"id": rec.ID, "merged": rec.Branch, "git": out, "cleaned": cleaned})
	return 0
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
	fs := flag.NewFlagSet("result", flag.ExitOnError)
	section := fs.String("section", "output", "output|meta|attempts")
	offset := fs.Int("offset", 0, "first line")
	limit := fs.Int("limit", 200, "max lines (0 = all)")
	pos := parseInterspersed(fs, args)
	if len(pos) != 1 {
		fatalUsage("result needs one run id")
	}
	rec, err := runsStore().Get(pos[0])
	if err != nil {
		fatal(fmt.Errorf("%s: %w", pos[0], err))
	}
	switch *section {
	case "output":
		content, total, more := runner.Page(rec.Output, *offset, *limit)
		printJSON(map[string]any{
			"id": rec.ID, "status": rec.Status, "section": "output",
			"offset": *offset, "limit": *limit, "totalLines": total, "hasMore": more, "content": content,
		})
	case "attempts":
		printJSON(map[string]any{"id": rec.ID, "status": rec.Status, "attempts": rec.Attempts})
	case "meta":
		rec.Output = ""
		printJSON(rec)
	default:
		fatalUsage("unknown section " + *section)
	}
	return 0
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
