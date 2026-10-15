package store

import (
	"path/filepath"
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
