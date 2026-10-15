package store

import (
	"path/filepath"
	"testing"
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
