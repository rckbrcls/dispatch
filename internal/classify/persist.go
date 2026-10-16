package classify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"dispatch/internal/adapter"
	"dispatch/internal/store"
)

// Load treats absent, obsolete, malformed and invalid models as a cold start.
// Other I/O errors return the prior together with the error.
func Load(path string) (Model, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return Prior(), nil
	}
	if err != nil {
		return Prior(), err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return Prior(), err
	}
	// Decode slices first: encoding/json silently pads/truncates fixed arrays.
	var wire struct {
		FeatureSchema int `json:"featureSchema"`
		Tiers         map[adapter.Tier]struct {
			Weights []float64 `json:"weights"`
			Samples uint64    `json:"samples"`
		} `json:"tiers"`
	}
	if json.Unmarshal(b, &wire) != nil || wire.FeatureSchema != featureSchema || len(wire.Tiers) != 3 {
		return Prior(), nil
	}
	m := Prior()
	for t, h := range wire.Tiers {
		if !t.Valid() || len(h.Weights) != featureCount {
			return Prior(), nil
		}
		var w Features
		copy(w[:], h.Weights)
		m.Tiers[t] = TierModel{Weights: w, Samples: h.Samples}
	}
	if !m.valid() {
		return Prior(), nil
	}
	return m, nil
}

// save is called under the stable sidecar lock. Rename keeps readers from
// observing a partial JSON document. The temporary file is private (0600).
func save(path string, m Model) error {
	if !m.valid() {
		return fmt.Errorf("classifier: invalid model")
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".classifier-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// mutate serializes concurrent processes/goroutines without holding a lock
// while agents run. A busy/unwritable state never stalls a run indefinitely.
func mutate(path string, fn func(*Model)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if (!errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN)) || time.Now().After(deadline) {
			return fmt.Errorf("classifier lock: %w", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // retain the sidecar inode
	m, err := Load(path)
	if err != nil {
		return err
	}
	fn(&m)
	return save(path, m)
}

func Reset(path string) error {
	return mutate(path, func(m *Model) { *m = Prior() })
}

// Observe consumes the immutable pre-run features, never re-extracting them
// from a repository that the agent may have changed. Old JSONL rows stay valid
// and are simply ineligible without features from the current schema.
func Observe(path string, r store.Record) error {
	failed, ok := Label(r)
	t := adapter.Tier(r.ExecutionTier)
	if !ok || !t.Valid() || r.FeatureSchema != featureSchema || len(r.ClassifyFeatures) != featureCount {
		return nil
	}
	var f Features
	copy(f[:], r.ClassifyFeatures)
	return mutate(path, func(m *Model) { m.Learn(t, f, failed) })
}
