package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dispatch/internal/adapter"
	"dispatch/internal/classify"
)

func classifyCmd(args []string) int {
	fs := flag.NewFlagSet("classify", flag.ContinueOnError)
	tier := fs.String("tier", "normal", "requested tier: easy|normal|hard")
	dir := fs.String("dir", "", "repository for cited paths (default current directory)")
	ro := fs.Bool("readonly", false, "read-only task context")
	asJSON := fs.Bool("json", false, "print JSON")
	explain := fs.Bool("explain", false, "show every normalized feature and contribution")
	reset := fs.Bool("reset", false, "reset all tier weights and counts to the prior")
	// Match run's interspersed flags, but return parsing errors instead of exit.
	var words []string
	for {
		if err := fs.Parse(args); err != nil {
			return 1
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		words = append(words, rest[0])
		args = rest[1:]
	}
	t := adapter.Tier(*tier)
	if !t.Valid() {
		fmt.Fprintln(os.Stderr, "classify: invalid tier (want easy|normal|hard)")
		return 1
	}
	prompt := strings.TrimSpace(strings.Join(words, " "))
	if prompt == "" && !*reset {
		fmt.Fprintln(os.Stderr, "classify: prompt required (or --reset)")
		return 1
	}
	path := filepath.Join(home(), "classifier.json")
	if *reset {
		if err := classify.Reset(path); err != nil {
			fmt.Fprintln(os.Stderr, "classify: could not reset classifier")
			return 1
		}
		if prompt == "" {
			if *asJSON {
				printJSON(map[string]any{"reset": true, "featureSchema": classify.FeatureSchema})
			} else {
				fmt.Println("classifier reset to prior")
			}
			return 0
		}
	}
	m, err := classify.Load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "classify: model unavailable; using prior")
	}
	p := m.Predict(t, classify.Extract(classify.Input{Prompt: prompt, Tier: t, Dir: *dir, ReadOnly: *ro}), *explain)
	if *asJSON {
		printJSON(p)
	} else {
		fmt.Printf("tier=%s score=%.3f requested=%s samples=%d featureSchema=%d\n", p.SuggestedTier, p.Score, p.RequestedTier, p.Samples, p.FeatureSchema)
		for _, r := range p.Reasons {
			fmt.Printf("  %s value=%.3f weight=%+.3f contribution=%+.3f\n", r.Feature, r.Value, r.Weight, r.Contribution)
		}
	}
	return 0
}
