package stats

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dispatch/internal/pace"
	"dispatch/internal/store"
)

func ms(n int64) *int64 { return &n }

func TestComputeRatesPercentilesAndRanks(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	mk := func(id, model, provider string, status store.Status, run *int64, attempts []store.Attempt, class string) store.Record {
		return store.Record{
			ID: id, Model: model, Provider: provider, Agent: "codex", Tier: "normal",
			Status: status, CreatedAt: now.Add(-time.Hour), RunMs: run, Attempts: attempts, ExitClass: class,
		}
	}
	var recs []store.Record
	for i, d := range []int64{10, 20, 30, 40, 50, 60} {
		recs = append(recs, mk("a"+string(rune('0'+i)), "gpt-5.6-terra", "openai", store.Completed, ms(d), nil, "ok"))
	}
	rl := []store.Attempt{{Agent: "grok", Model: "grok-4.7", Error: "429 too many requests", ExitClass: "rate_limit"}}
	recs = append(recs,
		mk("a6", "gpt-5.6-terra", "openai", store.Completed, ms(70), rl, "ok"),
		mk("a7", "gpt-5.6-terra", "openai", store.Completed, ms(80), rl, "ok"),
		mk("a8", "gpt-5.6-terra", "openai", store.Failed, ms(90), rl, "rate_limit"),
		mk("a9", "gpt-5.6-terra", "openai", store.Orphaned, nil, nil, ""),
	)
	for i := 0; i < 5; i++ {
		recs = append(recs, mk("b"+string(rune('0'+i)), "gemini-3.8-flash-low", "google", store.Failed, ms(200), nil, "failed"))
	}
	recs = append(recs, store.Record{ID: "fly", Status: store.Running, Model: "gpt-5.6-terra", CreatedAt: now})

	rep, err := Compute(recs, Options{By: "model", Since: now.Add(-7 * 24 * time.Hour), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Schema != Schema || rep.InFlight != 1 || rep.MinSamples != MinSamples {
		t.Fatalf("%+v", rep)
	}
	gpt := findRow(t, rep.Rows, "gpt-5.6-terra")
	if gpt.Runs != 10 || gpt.SuccessPct != 80 || gpt.FirstTryPct != 60 || gpt.Fallbacks != 3 || gpt.RateLimits != 3 || gpt.Orphans != 1 {
		t.Fatalf("%+v", gpt)
	}
	if gpt.P50Ms == nil || *gpt.P50Ms != 50 || gpt.P90Ms == nil || *gpt.P90Ms != 90 || gpt.DurationN != 9 {
		t.Fatalf("percentiles %+v", gpt)
	}
	gem := findRow(t, rep.Rows, "gemini-3.8-flash-low")
	if gem.Runs != 5 || gem.SuccessPct != 0 || gem.P50Ms == nil || *gem.P50Ms != 200 {
		t.Fatalf("%+v", gem)
	}
	if rep.Rankings.MostUsedModel.Key != "gpt-5.6-terra" || rep.Rankings.MostUsedModel.Runs != 10 {
		t.Fatalf("used %+v", rep.Rankings.MostUsedModel)
	}
	if rep.Rankings.MostUsedProvider.Key != "openai" || rep.Rankings.MostUsedProvider.Runs != 10 {
		t.Fatalf("provider %+v", rep.Rankings.MostUsedProvider)
	}
	if rep.Rankings.SlowestModel.Key != "gemini-3.8-flash-low" || rep.Rankings.SlowestModel.P50Ms == nil || *rep.Rankings.SlowestModel.P50Ms != 200 {
		t.Fatalf("slow %+v", rep.Rankings.SlowestModel)
	}
	if rep.Rankings.MostFailedModel.Key != "gemini-3.8-flash-low" || rep.Rankings.MostFailedModel.SuccessPct == nil || *rep.Rankings.MostFailedModel.SuccessPct != 0 {
		t.Fatalf("fail %+v", rep.Rankings.MostFailedModel)
	}
	text := rep.Text()
	if !strings.Contains(text, "gpt-5.6-terra") || !strings.Contains(text, "slowest model: gemini-3.8-flash-low") {
		t.Fatal(text)
	}

	// Too few samples to call a model slow or failure-prone.
	small, err := Compute([]store.Record{
		mk("s1", "fast", "openai", store.Completed, ms(1), nil, "ok"),
		mk("s2", "slow", "openai", store.Failed, ms(999), nil, "failed"),
	}, Options{By: "model", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if small.Rankings.SlowestModel.Key != "desconhecido" || !strings.Contains(small.Rankings.SlowestModel.Reason, "mínimo") {
		t.Fatalf("%+v", small.Rankings.SlowestModel)
	}
	if small.Rankings.MostFailedModel.Reason == "" || small.Rankings.MostUsedModel.Key == "" {
		t.Fatalf("%+v", small.Rankings)
	}
	if !strings.Contains(small.Text(), "desconhecido") {
		t.Fatal(small.Text())
	}
}

func TestUnknownGroupSinceAndZeroDuration(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	old := now.Add(-30 * 24 * time.Hour)
	recs := []store.Record{
		{ID: "e1", Status: store.Completed, Effort: "high", Model: "gemini-3.8-flash-high", CreatedAt: now, RunMs: ms(0)},
		{ID: "e2", Status: store.Failed, Model: "grok-4.7", CreatedAt: now},
		{ID: "old", Status: store.Completed, Model: "gpt-5.6-terra", CreatedAt: old, RunMs: ms(5)},
		{ID: "d1", Status: store.Completed, Model: "m", CreatedAt: time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)},
		{ID: "d2", Status: store.Completed, Model: "m", CreatedAt: time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)},
	}
	rep, err := Compute(recs, Options{By: "effort", Since: now.Add(-7 * 24 * time.Hour), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	high := findRow(t, rep.Rows, "high")
	if high.Runs != 1 || high.DurationN != 1 || high.P50Ms == nil || *high.P50Ms != 0 || high.Duration != "" {
		t.Fatalf("known zero %+v", high)
	}
	unk := findRow(t, rep.Rows, "desconhecido")
	if unk.Runs != 3 || unk.Duration != "desconhecido" || unk.P50Ms != nil {
		t.Fatalf("unknown %+v", unk)
	}
	// The 30-day-old run is outside the window. d1 and d2 and e2 have no effort.
	if len(rep.Rows) != 2 {
		t.Fatalf("rows %+v", rep.Rows)
	}

	day, err := Compute(recs[3:], Options{By: "day", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(day.Rows) != 2 || day.Rows[0].Key != "2026-10-14" || day.Rows[1].Key != "2026-10-15" {
		t.Fatalf("%+v", day.Rows)
	}
	week, err := Compute(recs[3:], Options{By: "week", Now: now})
	if err != nil || len(week.Rows) != 1 || !strings.HasPrefix(week.Rows[0].Key, "2026-W") {
		t.Fatalf("%+v %v", week.Rows, err)
	}
	if _, err := Compute(recs, Options{By: "nope"}); err == nil {
		t.Fatal("bad --by")
	}
	start, err := ParseSince("7d", now)
	if err != nil || !start.Equal(now.Add(-7*24*time.Hour)) {
		t.Fatal(start, err)
	}
	if start, err = ParseSince("all", now); err != nil || !start.IsZero() {
		t.Fatal(start, err)
	}
	if _, err := ParseSince("soon", now); err == nil {
		t.Fatal("expected error")
	}
}

func TestStatsReadsRotatedLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	archived := `{"id":"old","status":"COMPLETED","model":"gpt-5.6-terra","provider":"openai","createdAt":"2026-10-10T00:00:00Z","runMs":10}` + "\n"
	f, err := os.Create(path + ".20261010-000000Z.gz")
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte(archived)); err != nil {
		t.Fatal(err)
	}
	gz.Close()
	f.Close()
	live := `{"id":"new","status":"FAILED","model":"gpt-5.6-terra","provider":"openai","createdAt":"2026-10-14T00:00:00Z","runMs":20}` + "\n"
	if err := os.WriteFile(path, []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := FromStore(store.Store{Path: path}, Options{By: "model", Now: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	row := findRow(t, rep.Rows, "gpt-5.6-terra")
	if row.Runs != 2 || row.SuccessPct != 50 || row.DurationN != 2 {
		t.Fatalf("%+v", row)
	}
}

func TestCapacityWindows(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	snap := func(key string, used float64, reset, at time.Time) store.SnapshotRecord {
		return store.SnapshotRecord{Key: key, Snapshot: pace.Snapshot{
			UsedPercent: used, ResetsAt: reset, WindowMinutes: int(week / time.Minute), SampledAt: at,
		}}
	}
	reset1 := now.Add(-48 * time.Hour)
	reset2 := now.Add(-14 * 24 * time.Hour)
	reset3 := now.Add(72 * time.Hour)
	rep := Capacity([]store.SnapshotRecord{
		snap("codex", 40, reset1, reset1.Add(-time.Hour)),
		snap("codex", 100, reset2, reset2.Add(-30*time.Minute)),
		snap("codex", 80, reset3, now.Add(-time.Hour)),
	}, time.Time{}, now)
	if len(rep.Keys) != 1 {
		t.Fatalf("%+v", rep.Keys)
	}
	k := rep.Keys[0]
	if k.Windows != 3 || k.HitFull != 1 || k.LeftoverKnown != 2 || k.LeftoverUnknown != 1 {
		t.Fatalf("%+v", k)
	}
	if k.ConsumedPct == nil || *k.ConsumedPct != 73.3 || k.LeftoverPct == nil || *k.LeftoverPct != 30 {
		t.Fatalf("means consumed %v leftover %v", k.ConsumedPct, k.LeftoverPct)
	}
	if k.Pace != "adiantado" {
		t.Fatal(k.Pace)
	}
	var closed, open, full int
	for _, w := range k.Detail {
		if w.Open {
			open++
			if w.Leftover != "desconhecido" || w.LeftoverPct != nil || w.Pace != "adiantado" {
				t.Fatalf("open %+v", w)
			}
		} else {
			closed++
		}
		if w.HitFull {
			full++
			if w.LeftoverPct == nil || *w.LeftoverPct != 0 || w.Pace != "no ritmo" {
				t.Fatalf("full %+v", w)
			}
		}
		if !w.Open && !w.HitFull {
			if w.LeftoverPct == nil || *w.LeftoverPct != 60 || w.Pace != "atrasado" {
				t.Fatalf("partial %+v", w)
			}
		}
	}
	if closed != 2 || open != 1 || full != 1 {
		t.Fatalf("closed %d open %d full %d", closed, open, full)
	}
	if !strings.Contains(rep.Text(), "desconhecido") || !strings.Contains(rep.Note, "desconhecida") {
		t.Fatal(rep.Text())
	}

	empty := Capacity(nil, time.Time{}, now)
	if len(empty.Keys) != 0 || !strings.Contains(empty.Note, "desconhecido") {
		t.Fatalf("%+v", empty)
	}

	// 99.5 counts as touching 100; a sample far from the reset leaves leftover unknown.
	barely := now.Add(-time.Hour)
	early := barely.Add(-7 * 24 * time.Hour).Add(time.Hour)
	edge := Capacity([]store.SnapshotRecord{
		snap("codex", 99.5, barely, barely.Add(-time.Hour)),
		snap("grok", 10, barely, early),
	}, time.Time{}, now)
	if len(edge.Keys) != 2 || edge.Keys[0].Key != "codex" || edge.Keys[0].HitFull != 1 {
		t.Fatalf("%+v", edge.Keys)
	}
	grok := edge.Keys[1]
	if grok.Key != "grok" || grok.Leftover != "desconhecido" || grok.HitFull != 0 {
		t.Fatalf("%+v", grok)
	}
}

func findRow(t *testing.T, rows []Row, key string) Row {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("missing %s in %+v", key, rows)
	return Row{}
}

func TestEvidenceSegmentsByTierAndVerified(t *testing.T) {
	ms := func(n int64) *int64 { return &n }
	recs := []store.Record{
		{ID: "1", Status: store.Completed, Tier: "easy", Model: "m", Verification: "verified", RunMs: ms(1000)},
		{ID: "2", Status: store.Completed, Tier: "easy", Model: "m", Verification: "unverified", RunMs: ms(3000)},
		{ID: "3", Status: store.Completed, Tier: "hard", Model: "m", Verification: "verified", RunMs: ms(9000)},
		{ID: "4", Status: store.Running, Tier: "easy", Model: "m", Verification: "verified", RunMs: ms(1)},
		{ID: "5", Status: store.Completed, Tier: "easy", Model: "", Verification: "verified", RunMs: ms(1)},
	}
	ev := EvidenceByTierModel(recs)
	easy := ev[EvidenceKey("easy", "m")]
	hard := ev[EvidenceKey("hard", "m")]
	if easy.Samples != 2 || easy.Verified != 1 || easy.VerifiedRate != 0.5 || easy.DurationN != 2 {
		t.Fatalf("%+v", easy)
	}
	if easy.P50Ms == nil || *easy.P50Ms != 1000 || hard.P50Ms == nil || *hard.P50Ms != 9000 {
		t.Fatalf("easy %+v hard %+v", easy, hard)
	}
	if hard.Samples != 1 || hard.Verified != 1 || hard.VerifiedRate != 1 {
		t.Fatalf("%+v", hard)
	}
	if _, ok := ev[EvidenceKey("easy", "")]; ok {
		t.Fatal("empty model counted")
	}
}

func TestRoutingFilterAggregations(t *testing.T) {
	r := store.Record{ID: "filters", Status: store.Completed, Requested: &store.Filters{Provider: "google"}, Resolved: &store.Filters{Provider: "google", Model: "gemini-fast", Agent: "antigravity", Effort: "high"}}
	for _, by := range []string{"requested", "resolved"} {
		report, err := Compute([]store.Record{r}, Options{By: by})
		if err != nil || len(report.Rows) != 1 || !strings.Contains(report.Rows[0].Key, "google") {
			t.Fatalf("%+v %v", report, err)
		}
	}
}
