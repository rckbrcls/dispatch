package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Antigravity drives the Antigravity CLI (agy). Effort is part of the model
// ID (e.g. gemini-3.8-flash-low). Tool permissions come from the user's
// agy settings; denied tools are surfaced as errors. Quota comes from agy's
// own non-interactive `/usage` report, grouped by model family.
type Antigravity struct{}

var antigravityRun = cliAdapter{
	name: "antigravity", binary: "agy",
	caps:        Capabilities{SessionResume: true, StructuredOutput: true, ReadOnly: true, FileEditing: true},
	listArgs:    []string{"models"},
	parseModels: parseAgyModels,
	setDir:      true,
	parse:       parseAgyJSON,
	args: func(r Request, model, session string, resume bool) []string {
		a := []string{"--output-format", "json", "--sandbox", "--disable-slash-commands"}
		if r.ReadOnly {
			a = append(a, "--mode", "plan")
		} else {
			a = append(a, "--mode", "accept-edits")
		}
		if resume {
			a = append(a, "--conversation", session)
		}
		if model != "" {
			a = append(a, "--model", model)
		}
		// agy requires the prompt immediately after -p, so it goes last.
		return append(a, "-p", r.Prompt)
	},
}

func (Antigravity) Name() string               { return "antigravity" }
func (Antigravity) Binary() string             { return "agy" }
func (Antigravity) Capabilities() Capabilities { return antigravityRun.caps }
func (Antigravity) Models(ctx context.Context) ([]Model, error) {
	return antigravityRun.Models(ctx)
}
func (Antigravity) Run(ctx context.Context, r Request, m string) (Result, error) {
	return antigravityRun.Run(ctx, r, m)
}

// Quota is unavailable without a model: use ModelQuotas.
func (Antigravity) Quota(context.Context) (Quota, error) { return Quota{}, ErrNoQuota }

// ModelQuotas resolves each model to its group's quota.
func (Antigravity) ModelQuotas(ctx context.Context, models []string) (map[string]Quota, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "agy", "-p", "/usage", "--output-format", "json")
	ownGroup(cmd)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoQuota, err)
	}
	groups, err := parseAgyUsage(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoQuota, err)
	}
	res := map[string]Quota{}
	for _, m := range models {
		g := "Claude and GPT models"
		if strings.Contains(strings.ToLower(m), "gemini") {
			g = "Gemini Models"
		}
		if q, ok := groups[g]; ok {
			q.Key = "antigravity:" + g
			res[m] = q
		}
	}
	return res, nil
}

// parseAgyUsage turns the /usage JSON into one Quota per group. It uses the
// weekly bucket, unless the 5-hour bucket is nearly exhausted (>= 90% used),
// in which case that bucket is the binding constraint.
func parseAgyUsage(raw []byte) (map[string]Quota, error) {
	var env struct {
		Command struct {
			Data struct {
				Groups []struct {
					Name    string `json:"name"`
					Buckets []struct {
						Window            string  `json:"window"`
						RemainingFraction float64 `json:"remaining_fraction"`
						ResetTime         string  `json:"reset_time"`
					} `json:"buckets"`
				} `json:"groups"`
			} `json:"data"`
		} `json:"command"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	out := map[string]Quota{}
	for _, g := range env.Command.Data.Groups {
		var weekly, fiveH *Quota
		for _, b := range g.Buckets {
			reset, err := time.Parse(time.RFC3339, b.ResetTime)
			if err != nil {
				continue
			}
			q := Quota{UsedPercent: 100 * (1 - b.RemainingFraction), ResetsAt: reset}
			switch b.Window {
			case "weekly":
				q.WindowMinutes = 7 * 24 * 60
				weekly = &q
			case "5h":
				q.WindowMinutes = 5 * 60
				fiveH = &q
			}
		}
		chosen := weekly
		if fiveH != nil && (weekly == nil || fiveH.UsedPercent >= 90) {
			chosen = fiveH
		}
		if chosen != nil {
			out[g.Name] = *chosen
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usage groups in report")
	}
	return out, nil
}
