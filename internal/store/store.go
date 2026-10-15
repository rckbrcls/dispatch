// Package store persists run records as append-only JSONL. Each state
// change appends a full record; the latest line per ID wins.
package store

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type Status string

const (
	Pending   Status = "PENDING"
	Running   Status = "RUNNING"
	Completed Status = "COMPLETED"
	Failed    Status = "FAILED"
	Cancelled Status = "CANCELLED"
	TimedOut  Status = "TIMED_OUT"
)

type Attempt struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
	Error string `json:"error"`
}

type Record struct {
	ID         string    `json:"id"`
	Status     Status    `json:"status"`
	Tier       string    `json:"tier"`
	Agent      string    `json:"agent,omitempty"`
	Model      string    `json:"model,omitempty"`
	Prompt     string    `json:"prompt"`
	Dir        string    `json:"dir,omitempty"`
	ReadOnly   bool      `json:"readOnly"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	ExitCode   int       `json:"exitCode"`
	Attempts   []Attempt `json:"attempts,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

type Store struct{ Path string }

var ErrNotFound = errors.New("run not found")

func NewID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

func (s Store) Append(r Record) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
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

func (s Store) latest() (map[string]Record, error) {
	f, err := os.Open(s.Path)
	if os.IsNotExist(err) {
		return map[string]Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]Record{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.ID != "" {
			out[r.ID] = r
		}
	}
	return out, sc.Err()
}
