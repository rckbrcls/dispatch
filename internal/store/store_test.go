package store

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dispatch/internal/pace"
)

func TestLatestWins(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "runs.jsonl")}
	s.Append(Record{ID: "abc-1", Status: Pending})
	s.Append(Record{ID: "abc-1", Status: Completed, Output: "done"})
	r, err := s.Get("abc-1")
	if err != nil || r.Status != Completed || r.Output != "done" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := s.Get("abc"); err != nil || r.ID != "abc-1" {
		t.Fatalf("prefix: %+v %v", r, err)
	}
	if _, err := s.Get("zzz"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSnapshotSamplingAndCooldown(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "runs.jsonl")}
	t0 := time.Now()
	reset := t0.Add(48 * time.Hour)
	snap := func(used float64, at time.Time) pace.Snapshot {
		return pace.Snapshot{UsedPercent: used, ResetsAt: reset, WindowMinutes: 10080, SampledAt: at}
	}
	if ok, _ := s.RecordSnapshot("k", snap(10, t0)); !ok {
		t.Fatal("first sample must be kept")
	}
	if ok, _ := s.RecordSnapshot("k", snap(10.2, t0.Add(5*time.Minute))); ok {
		t.Fatal("tiny change soon after must be skipped")
	}
	if ok, _ := s.RecordSnapshot("k", snap(12, t0.Add(6*time.Minute))); !ok {
		t.Fatal(">=1pt change must be kept")
	}
	if ok, _ := s.RecordSnapshot("k", snap(12, t0.Add(40*time.Minute))); !ok {
		t.Fatal("30 min later must be kept")
	}
	if n := len(s.Snapshots("k")); n != 3 {
		t.Fatalf("got %d snapshots", n)
	}
	if n := len(s.Snapshots("other")); n != 0 {
		t.Fatalf("got %d", n)
	}

	s.SetCooldown("codex", t0.Add(time.Hour), t0)
	s.SetCooldown("grok", t0.Add(-time.Hour), t0)
	c := s.Cooldowns(t0)
	if _, ok := c["codex"]; !ok || len(c) != 1 {
		t.Fatalf("%v", c)
	}
}

func TestStoreAppendRedactsSecrets(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "runs.jsonl")}
	rec := Record{
		ID:     "sec-1",
		Status: Failed,
		Output: "ran with GITHUB_TOKEN=ghp_secret12345",
		Error:  "failed: Authorization: Bearer secret_bearer_token",
		Note:   "note: AWS_SECRET_ACCESS_KEY=my_secret_aws_key",
		Attempts: []Attempt{
			{Agent: "a", Model: "m", Error: "attempt error with DB_PASSWORD=my_db_pass"},
		},
	}
	if err := s.Append(rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("sec-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Output != "ran with GITHUB_TOKEN=[REDACTED]" {
		t.Errorf("output not redacted: %q", got.Output)
	}
	if got.Error != "failed: Authorization: [REDACTED]" {
		t.Errorf("error not redacted: %q", got.Error)
	}
	if got.Note != "note: AWS_SECRET_ACCESS_KEY=[REDACTED]" {
		t.Errorf("note not redacted: %q", got.Note)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].Error != "attempt error with DB_PASSWORD=[REDACTED]" {
		t.Errorf("attempt error not redacted: %+v", got.Attempts)
	}
}

func TestOptionalZeroIsNotUnknown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	s := Store{Path: path}
	z := 0
	if err := s.Append(Record{
		ID: "z", Status: Completed, FilesChanged: &z, Insertions: &z, Deletions: &z, RunMs: I64(0),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("z")
	if err != nil || got.FilesChanged == nil || *got.FilesChanged != 0 || got.RunMs == nil || *got.RunMs != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"filesChanged":0`) || !strings.Contains(string(b), `"runMs":0`) {
		t.Fatal(string(b))
	}

	legacy := `{"id":"legacy","status":"COMPLETED","agent":"codex","model":"gpt-5.6-terra","createdAt":"2026-01-02T00:00:00Z","finishedAt":"2026-01-02T00:00:05Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "other.jsonl"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	old := Store{Path: filepath.Join(dir, "other.jsonl")}
	leg, err := old.Get("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if leg.RunMs != nil || leg.FilesChanged != nil || leg.QueuedMs != nil || leg.Provider != "" || leg.Effort != "" {
		t.Fatalf("legacy gained fields: %+v", leg)
	}
	ms, ok := leg.ObservedMs()
	if !ok || ms != 5000 {
		t.Fatalf("derived duration %d %v", ms, ok)
	}
}

func TestRotateReadsGzipAndLatestWins(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runs.jsonl")
	prev := rotateBytes
	rotateBytes = 200
	t.Cleanup(func() { rotateBytes = prev })

	s := Store{Path: path}
	if err := s.Append(Record{ID: "old", Status: Completed, Output: strings.Repeat("y", 500)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Record{ID: "new", Status: Failed, Output: "n"}); err != nil {
		t.Fatal(err)
	}
	gzs := 0
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "runs.jsonl.") && strings.HasSuffix(e.Name(), ".gz") {
			gzs++
		}
	}
	if gzs != 1 {
		t.Fatalf("gzip archives = %d", gzs)
	}
	live, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(live), `"id":"old"`) {
		t.Fatalf("live file should only have the new row: %s %v", live, err)
	}
	oldRec, err := s.Get("old")
	if err != nil || !strings.Contains(oldRec.Output, "yyy") {
		t.Fatalf("%+v %v", oldRec, err)
	}
	if _, err := s.Get("new"); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("%d %v", len(list), err)
	}

	// A later live line overrides the archived copy of the same id.
	archived := `{"id":"same","status":"FAILED","output":"archived"}` + "\n"
	f, err := os.Create(filepath.Join(dir, "runs.jsonl.20260101-000000Z.gz"))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	if _, err := gz.Write([]byte(archived)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.WriteFile(path, []byte(`{"id":"same","status":"COMPLETED","output":"live"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("same")
	if err != nil || got.Status != Completed || got.Output != "live" {
		t.Fatalf("latest live line should win: %+v %v", got, err)
	}
}

func TestConcurrentAppendWithRotationLosesNothing(t *testing.T) {
	prev := rotateBytes
	rotateBytes = 2000
	t.Cleanup(func() { rotateBytes = prev })
	s := Store{Path: filepath.Join(t.TempDir(), "runs.jsonl")}
	const n = 60
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Append(Record{ID: fmt.Sprintf("r-%03d", i), Status: Completed, Output: strings.Repeat("x", 300)}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	recs, err := s.List()
	if err != nil || len(recs) != n {
		t.Fatalf("got %d records, want %d (err %v)", len(recs), n, err)
	}
}

func TestRateLimitReIsSpecific(t *testing.T) {
	for _, s := range []string{"429 Too Many Requests", "rate limit reached", "You exceeded your current quota", "usage limit hit", "RESOURCE_EXHAUSTED", "insufficient_quota"} {
		if !RateLimitRe.MatchString(s) {
			t.Errorf("should match %q", s)
		}
	}
	for _, s := range []string{"all 2 candidates exhausted", "quota file missing", "port 14290 busy", "disk space exhausted"} {
		if RateLimitRe.MatchString(s) {
			t.Errorf("should not match %q", s)
		}
	}
}

func TestGetLatestLineExactAndPrefix(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "runs.jsonl")}
	for _, r := range []Record{
		{ID: "20260101-aaaa1111", Status: Running},
		{ID: "20260101-bbbb2222", Status: Completed},
		{ID: "20260101-aaaa1111", Status: Failed},
	} {
		if err := s.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if r, err := s.Get("20260101-aaaa1111"); err != nil || r.Status != Failed {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := s.Get("20260101-bbbb"); err != nil || r.ID != "20260101-bbbb2222" {
		t.Fatalf("prefix: %+v %v", r, err)
	}
	if _, err := s.Get("20260101"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want ambiguous, got %v", err)
	}
	if _, err := s.Get("zzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestClearCooldown(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "runs.jsonl")}
	now := time.Now()
	for _, a := range []string{"codex", "grok"} {
		if err := s.SetCooldown(a, now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ClearCooldown("codex", now); err != nil {
		t.Fatal(err)
	}
	if c := s.Cooldowns(now); len(c) != 1 || c["grok"].IsZero() {
		t.Fatalf("after clearing codex: %v", c)
	}
	if err := s.ClearCooldown("", now); err != nil || len(s.Cooldowns(now)) != 0 {
		t.Fatalf("clear all: %v %v", err, s.Cooldowns(now))
	}
}
