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
	"strings"
	"syscall"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/runner"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
)

const usageText = `usage:
  dispatch agents [--json]
  dispatch tiers [--init]
  dispatch run --tier easy|normal|hard [--dir D] [--readonly] [--timeout 10m] <prompt | ->
  dispatch result <id> [--section output|meta|attempts] [--offset N] [--limit N]

state lives in $DISPATCH_HOME (default ~/.dispatch): runs.jsonl, tiers.json`

func main() {
	if len(os.Args) < 2 {
		fatalUsage("")
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "agents":
		agents(args)
	case "tiers":
		tiersCmd(args)
	case "run":
		os.Exit(run(args))
	case "result":
		os.Exit(result(args))
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
	fmt.Fprintln(os.Stderr, "dispatch:", err)
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
	printJSON(t)
}

// --- run ---

func run(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	tier := fs.String("tier", "normal", "easy|normal|hard")
	dir := fs.String("dir", "", "working directory for the agent")
	ro := fs.Bool("readonly", false, "forbid file edits")
	timeout := fs.Duration("timeout", 0, "max run time (0 = none)")
	fs.Parse(args)

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" || prompt == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fatal(err)
		}
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		fatalUsage("empty prompt")
	}
	table, err := tiers.Load(tiersPath())
	if err != nil {
		fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	d := runner.Deps{Adapters: adapter.Known(), Table: table, Store: runsStore(), Now: time.Now}
	rec, err := d.Run(ctx, runner.Options{
		Tier: adapter.Tier(*tier), Prompt: prompt, Dir: *dir, ReadOnly: *ro, Timeout: *timeout,
	})
	if err != nil && rec.ID == "" {
		fatal(err)
	}
	sum, truncated := runner.Summary(rec.Output, 40, 4000)
	_, total, _ := runner.Page(rec.Output, 0, 0)
	printJSON(map[string]any{
		"id": rec.ID, "status": rec.Status, "agent": rec.Agent, "model": rec.Model,
		"summary": sum, "truncated": truncated, "totalLines": total,
		"error": rec.Error, "attempts": len(rec.Attempts),
	})
	switch rec.Status {
	case store.Completed:
		return 0
	case store.TimedOut:
		return 124
	default:
		return 1
	}
}

// --- result ---

func result(args []string) int {
	fs := flag.NewFlagSet("result", flag.ExitOnError)
	section := fs.String("section", "output", "output|meta|attempts")
	offset := fs.Int("offset", 0, "first line")
	limit := fs.Int("limit", 200, "max lines (0 = all)")
	// Allow the id before or after flags.
	var id string
	var rest []string
	for i, a := range args {
		if !strings.HasPrefix(a, "-") && id == "" && (i == 0 || !strings.HasPrefix(args[i-1], "-") || strings.Contains(args[i-1], "=")) {
			id = a
			continue
		}
		rest = append(rest, a)
	}
	fs.Parse(rest)
	if id == "" {
		fatalUsage("result needs a run id")
	}
	rec, err := runsStore().Get(id)
	if err != nil {
		fatal(fmt.Errorf("%s: %w", id, err))
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
