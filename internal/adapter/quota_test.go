package adapter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEmpiricalSampleOmitsPercent(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	cool := EmpiricalSample(now, now.Add(time.Hour), 3)
	if cool.Used != nil || cool.Risk != "cooldown" || cool.Confidence != ConfidenceEmpirical || cool.Source != SourceEmpirical {
		t.Fatalf("%+v", cool)
	}
	if cool.ResetAt.IsZero() {
		t.Fatal("cooldown should carry resetAt")
	}
	recent := EmpiricalSample(now, time.Time{}, 2)
	if recent.Used != nil || recent.Risk != "recent_rate_limit" || !recent.ResetAt.IsZero() {
		t.Fatalf("%+v", recent)
	}
	none := EmpiricalSample(now, time.Time{}, 0)
	if none.Used != nil || none.Risk != "unknown" {
		t.Fatalf("%+v", none)
	}
	for _, s := range []QuotaSample{cool, recent, none} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"used"`) {
			t.Fatalf("invented percent: %s", b)
		}
	}
}

func TestCodexQuotaSamples(t *testing.T) {
	mins := func(n int) *int { return &n }
	unix := func(n int64) *int64 { return &n }
	primary := &codexWindow{UsedPercent: 10, WindowDurationMins: mins(300), ResetsAt: unix(1_780_000_000)}
	secondary := &codexWindow{UsedPercent: 40, WindowDurationMins: mins(10080), ResetsAt: unix(1_780_000_000)}
	q, err := codexQuota(primary, secondary)
	if err != nil || q.UsedPercent != 40 || q.WindowMinutes != 10080 || q.Key != "codex" {
		t.Fatalf("%+v %v", q, err)
	}
	samples, err := codexSamples(primary, secondary)
	if err != nil || len(samples) != 2 {
		t.Fatal(err, samples)
	}
	if samples[0].Window != "primary" || samples[1].Window != "secondary" {
		t.Fatalf("%+v", samples)
	}
	if samples[1].Source != SourceCodexAppServer || samples[1].Confidence != ConfidenceOfficial || samples[1].Used == nil || *samples[1].Used != 40 {
		t.Fatalf("%+v", samples[1])
	}
	if samples[1].Duration.Duration() != 10080*time.Minute {
		t.Fatalf("duration %s", samples[1].Duration.Duration())
	}
	b, err := json.Marshal(samples[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"window"`, `"used"`, `"resetAt"`, `"duration"`, `"source"`, `"confidence"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("missing %s in %s", key, b)
		}
	}
	incomplete := &codexWindow{UsedPercent: 1}
	if _, err := codexQuota(primary, incomplete); err != ErrNoQuota {
		t.Fatalf("incomplete secondary must not fall back: %v", err)
	}
	q, err = codexQuota(primary, nil)
	if err != nil || q.UsedPercent != 10 || q.WindowMinutes != 300 {
		t.Fatalf("%+v %v", q, err)
	}
}

func TestParseAgySamples(t *testing.T) {
	raw := []byte(`{"command":{"data":{"groups":[
	 {"name":"Gemini Models","buckets":[
	  {"window":"weekly","remaining_fraction":0.97,"reset_time":"2026-10-20T18:45:17Z"},
	  {"window":"5h","remaining_fraction":1,"reset_time":"2026-10-15T08:05:33Z"}]},
	 {"name":"Claude and GPT models","buckets":[
	  {"window":"weekly","remaining_fraction":0.5,"reset_time":"2026-10-22T03:17:31Z"},
	  {"window":"5h","remaining_fraction":0.05,"reset_time":"2026-10-15T08:17:31Z"}]}]}}}`)
	samples, err := parseAgySamples(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 4 {
		t.Fatalf("%d samples", len(samples))
	}
	by := map[string]QuotaSample{}
	for _, s := range samples {
		if s.Used == nil || s.Source != SourceAgyUsage || s.Confidence != ConfidenceOfficial {
			t.Fatalf("%+v", s)
		}
		by[s.Window] = s
	}
	if by["Gemini Models/5h"].Duration.Duration() != 5*time.Hour || *by["Gemini Models/5h"].Used != 0 {
		t.Fatalf("%+v", by["Gemini Models/5h"])
	}
	if by["Claude and GPT models/weekly"].Duration.Duration() != 7*24*time.Hour || *by["Claude and GPT models/weekly"].Used != 50 {
		t.Fatalf("%+v", by["Claude and GPT models/weekly"])
	}
	b, _ := json.Marshal(by["Gemini Models/5h"])
	if !strings.Contains(string(b), `"used":0`) {
		t.Fatalf("official zero must be present: %s", b)
	}
}
