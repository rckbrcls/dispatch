package store

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dispatch/internal/pace"
)

const (
	snapshotEvery = 30 * time.Minute
	snapshotDelta = 1.0 // percentage points
	retention     = 56 * 24 * time.Hour
	compactAbove  = 1 << 20 // bytes
)

func (s Store) dir() string { return filepath.Dir(s.Path) }

type snapRow struct {
	Key string `json:"key"`
	pace.Snapshot
}

// RecordSnapshot appends a quota observation for key when it is worth
// keeping: first sample, 30 min since the last, >=1 point change, or a new
// reset time. It reports whether a row was written.
func (s Store) RecordSnapshot(key string, snap pace.Snapshot) (bool, error) {
	prev := s.Snapshots(key)
	if n := len(prev); n > 0 {
		last := prev[n-1]
		if snap.SampledAt.Sub(last.SampledAt) < snapshotEvery &&
			math.Abs(snap.UsedPercent-last.UsedPercent) < snapshotDelta &&
			snap.ResetsAt.Round(time.Hour).Equal(last.ResetsAt.Round(time.Hour)) {
			return false, nil
		}
	}
	path := filepath.Join(s.dir(), "snapshots.jsonl")
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return false, err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > compactAbove {
		s.compactSnapshots(path, snap.SampledAt)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer f.Close()
	b, err := json.Marshal(snapRow{Key: key, Snapshot: snap})
	if err != nil {
		return false, err
	}
	_, err = f.Write(append(b, '\n'))
	return err == nil, err
}

// SnapshotRecord is one row of snapshots.jsonl.
type SnapshotRecord struct {
	Key string `json:"key"`
	pace.Snapshot
}

// AllSnapshots reads every retained quota row. A missing file is empty.
func (s Store) AllSnapshots() ([]SnapshotRecord, error) {
	f, err := os.Open(filepath.Join(s.dir(), "snapshots.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []SnapshotRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		var r SnapshotRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Key != "" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// Snapshots returns the retained snapshots for key, oldest first.
func (s Store) Snapshots(key string) []pace.Snapshot {
	f, err := os.Open(filepath.Join(s.dir(), "snapshots.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []pace.Snapshot
	cutoff := time.Now().Add(-retention)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		var r snapRow
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Key == key && r.SampledAt.After(cutoff) {
			out = append(out, r.Snapshot)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SampledAt.Before(out[j].SampledAt) })
	return out
}

// compactSnapshots rewrites the file dropping rows past retention.
func (s Store) compactSnapshots(path string, now time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	var keep [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		var r snapRow
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.SampledAt.After(now.Add(-retention)) {
			keep = append(keep, append([]byte(nil), sc.Bytes()...))
		}
	}
	f.Close()
	var out []byte
	for _, l := range keep {
		out = append(append(out, l...), '\n')
	}
	os.WriteFile(path+".tmp", out, 0o644)
	os.Rename(path+".tmp", path)
}

// Cooldowns maps an agent to the time until which it should be avoided
// (learned from rate-limit errors). Expired entries are omitted.
func (s Store) Cooldowns(now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	b, err := os.ReadFile(filepath.Join(s.dir(), "cooldowns.json"))
	if err != nil {
		return out
	}
	var all map[string]time.Time
	if json.Unmarshal(b, &all) != nil {
		return out
	}
	for k, until := range all {
		if until.After(now) {
			out[k] = until
		}
	}
	return out
}

func (s Store) SetCooldown(agent string, until, now time.Time) error {
	c := s.Cooldowns(now)
	c[agent] = until
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir(), "cooldowns.json"), b, 0o644)
}

// ClearCooldown drops the cooldown of one agent, or of every agent when
// agent is empty.
func (s Store) ClearCooldown(agent string, now time.Time) error {
	c := s.Cooldowns(now)
	if agent == "" {
		c = map[string]time.Time{}
	} else {
		for k := range c {
			if k == agent || strings.HasPrefix(k, agent+":") { // agent or agent:provider
				delete(c, k)
			}
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dir(), "cooldowns.json"), b, 0o644)
}
