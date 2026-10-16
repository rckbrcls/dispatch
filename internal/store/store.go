// Package store persists run records as append-only JSONL. Each state
// change appends a full record; the latest line per ID wins.
package store

import (
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dispatch/internal/sandbox"
	"dispatch/internal/verify"
)

type Status string

const (
	Orphaned  Status = "ORPHANED"
	Pending   Status = "PENDING"
	Running   Status = "RUNNING"
	Completed Status = "COMPLETED"
	Failed    Status = "FAILED"
	Cancelled Status = "CANCELLED"
	TimedOut  Status = "TIMED_OUT"
)

type Attempt struct {
	Agent      string `json:"agent"`
	Model      string `json:"model"`
	Error      string `json:"error"`
	DurationMs *int64 `json:"durationMs,omitempty"`
	ExitClass  string `json:"exitClass,omitempty"` // rate_limit|crash|merge_conflict|failed
}

type Filters struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Relax    bool   `json:"relax,omitempty"`
	// Priority is speed, balanced, or quality. Empty on old rows means unknown.
	Priority string `json:"priority,omitempty"`
}

type Record struct {
	Requested    *Filters       `json:"requested,omitempty"`
	Resolved     *Filters       `json:"resolved,omitempty"`
	ID           string         `json:"id"`
	Status       Status         `json:"status"`
	Tier         string         `json:"tier"`
	Agent        string         `json:"agent,omitempty"`
	Model        string         `json:"model,omitempty"`
	Provider     string         `json:"provider,omitempty"`
	Effort       string         `json:"effort,omitempty"` // suffix of the model name; absent = unknown
	Prompt       string         `json:"prompt"`
	Dir          string         `json:"dir,omitempty"`
	ReadOnly     bool           `json:"readOnly"`
	Output       string         `json:"output,omitempty"`
	Error        string         `json:"error,omitempty"`
	ExitCode     int            `json:"exitCode"`
	Attempts     []Attempt      `json:"attempts,omitempty"`
	TimeoutSec   int            `json:"timeoutSec,omitempty"`
	NoWorktree   bool           `json:"noWorktree,omitempty"`
	Profile      string         `json:"profile,omitempty"`
	Prefer       []string       `json:"prefer,omitempty"`
	Avoid        []string       `json:"avoid,omitempty"`
	StartedAt    time.Time      `json:"startedAt,omitempty,omitzero"`
	AgentPID     int            `json:"agentPid,omitempty"`
	ProcStart    string         `json:"procStart,omitempty"`
	BaseSHA      string         `json:"baseSHA,omitempty"`
	PID          int            `json:"pid,omitempty"`
	SessionID    string         `json:"sessionId,omitempty"`
	ResumedOf    string         `json:"resumedOf,omitempty"`
	Repo         string         `json:"repo,omitempty"`     // repo root when a worktree was made
	Worktree     string         `json:"worktree,omitempty"` // path the agent ran in
	Branch       string         `json:"branch,omitempty"`
	Note         string         `json:"note,omitempty"`
	NoVerify     bool           `json:"noVerify,omitempty"`
	Verification string         `json:"verification,omitempty"` // verified|verification_failed|unverified|error
	Verify       *verify.Result `json:"verify,omitempty"`
	VerifyMs     int64          `json:"verifyMs,omitempty"`
	QueuedMs     *int64         `json:"queuedMs,omitempty"` // nil = unknown, never a sentinel zero
	RunMs        *int64         `json:"runMs,omitempty"`
	FilesChanged *int           `json:"filesChanged,omitempty"`
	Insertions   *int           `json:"insertions,omitempty"`
	Deletions    *int           `json:"deletions,omitempty"`
	ExitClass    string         `json:"exitClass,omitempty"` // ok|verify_fail|timeout|cancelled|failed|rate_limit|crash|merge_conflict
	CreatedAt    time.Time      `json:"createdAt"`
	FinishedAt   time.Time      `json:"finishedAt,omitzero"`

	// Optional classifier telemetry; old JSONL records have no feature snapshot.
	RequestedTier    string    `json:"requestedTier,omitempty"`
	ExecutionTier    string    `json:"executionTier,omitempty"`
	FeatureSchema    int       `json:"featureSchema,omitempty"`
	ClassifyFeatures []float64 `json:"classifyFeatures,omitempty"`
}

// I64 returns a pointer so a known zero is stored and stays distinct from absent.
func I64(n int64) *int64 { return &n }

// I returns a pointer so a known zero is stored and stays distinct from absent.
func I(n int) *int { return &n }

// ObservedMs is the run duration when it is known: RunMs, else the span from
// StartedAt or CreatedAt to FinishedAt. The boolean is false when nothing was recorded.
func (r Record) ObservedMs() (int64, bool) {
	if r.RunMs != nil {
		return *r.RunMs, true
	}
	if !r.StartedAt.IsZero() && !r.FinishedAt.IsZero() {
		return nonNegMs(r.FinishedAt.Sub(r.StartedAt)), true
	}
	if !r.CreatedAt.IsZero() && !r.FinishedAt.IsZero() {
		return nonNegMs(r.FinishedAt.Sub(r.CreatedAt)), true
	}
	return 0, false
}

func nonNegMs(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

type Store struct{ Path string }

var ErrNotFound = errors.New("run not found")

func NewID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func (s Store) Append(r Record) error {
	r.Output = sandbox.Redact(r.Output)
	r.Error = sandbox.Redact(r.Error)
	r.Note = sandbox.Redact(r.Note)
	for i := range r.Attempts {
		r.Attempts[i].Error = sandbox.Redact(r.Attempts[i].Error)
	}
	if r.Verify != nil {
		for i := range r.Verify.Checks {
			r.Verify.Checks[i].Tail = sandbox.Redact(r.Verify.Checks[i].Tail)
		}
		for i, w := range r.Verify.Warnings {
			r.Verify.Warnings[i] = sandbox.Redact(w)
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	if err := s.rotate(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Get returns the latest record for id. An id prefix is accepted when unique.
func (s Store) Get(id string) (Record, error) {
	all, err := s.latest()
	if err != nil {
		return Record{}, err
	}
	if r, ok := all[id]; ok {
		return r, nil
	}
	var found *Record
	for k, r := range all {
		if len(id) > 0 && len(k) >= len(id) && k[:len(id)] == id {
			if found != nil {
				return Record{}, errors.New("ambiguous run id prefix")
			}
			r := r
			found = &r
		}
	}
	if found == nil {
		return Record{}, ErrNotFound
	}
	return *found, nil
}

// List returns the latest record of every run, newest first.
func (s Store) List() ([]Record, error) {
	all, err := s.latest()
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(all))
	for _, r := range all {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// rotateBytes is the runs.jsonl size that triggers gzip rotation.
// Tests lower it; the default is 5 MiB.
var rotateBytes int64 = 5 << 20

// rotate gzips runs.jsonl once it reaches rotateBytes. The archived name is
// "<path>.<UTC stamp>Z.gz". Readers must open those files too.
func (s Store) rotate() error {
	st, err := os.Stat(s.Path)
	if err != nil || st.Size() < rotateBytes {
		return nil
	}
	stamp := time.Now().UTC().Format("20060102-150405") + "Z"
	dest := s.Path + "." + stamp + ".gz"
	for n := 1; ; n++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		if n > 1000 {
			return fmt.Errorf("rotate %s: too many files", s.Path)
		}
		dest = fmt.Sprintf("%s.%s.%d.gz", s.Path, stamp, n)
	}
	in, err := os.Open(s.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		gz.Close()
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := gz.Close(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(s.Path)
}

// historyPaths returns rotated gzip archives oldest-first, then the live file.
func (s Store) historyPaths() ([]string, error) {
	dir := filepath.Dir(s.Path)
	base := filepath.Base(s.Path)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rotated []string
	for _, e := range entries {
		name := e.Name()
		if name == base || e.IsDir() {
			continue
		}
		if strings.HasPrefix(name, base+".") && strings.HasSuffix(name, ".gz") {
			rotated = append(rotated, filepath.Join(dir, name))
		}
	}
	sort.Strings(rotated)
	if _, err := os.Stat(s.Path); err == nil {
		rotated = append(rotated, s.Path)
	}
	return rotated, nil
}

func (s Store) latest() (map[string]Record, error) {
	paths, err := s.historyPaths()
	if err != nil {
		return nil, err
	}
	out := map[string]Record{}
	for _, p := range paths {
		if err := readRecords(p, func(r Record) { out[r.ID] = r }); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readRecords(path string, fn func(Record)) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var rec Record
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.ID != "" {
			fn(rec)
		}
	}
	return sc.Err()
}
