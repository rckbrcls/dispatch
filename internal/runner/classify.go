package runner

import (
	"fmt"
	"path/filepath"

	"dispatch/internal/adapter"
	"dispatch/internal/classify"
	"dispatch/internal/store"
)

func (d Deps) classifierPath() string {
	return filepath.Join(filepath.Dir(d.Store.Path), "classifier.json")
}

func (d Deps) classifyRequest(rec *store.Record, promote bool) {
	t := adapter.Tier(rec.Tier)
	if !t.Valid() {
		return // provider/model-only runs have no known requested difficulty
	}
	rec.RequestedTier = rec.Tier
	in := classify.Input{Prompt: rec.Prompt, Dir: rec.Dir, ReadOnly: rec.ReadOnly, Tier: t}
	f := classify.Extract(in)
	m, _ := classify.Load(d.classifierPath()) // an I/O error also returns the prior
	p := m.Predict(t, f, false)
	if promote && f.Sensitive() && classify.Higher(t) != t {
		rec.Tier = string(classify.Higher(t))
		rec.Note = joinNote(rec.Note, fmt.Sprintf("sensitive code: tier %s -> %s", t, rec.Tier))
	} else if p.SuggestedTier != t {
		rec.Note = joinNote(rec.Note, fmt.Sprintf("classifier warning: suggests tier %s above %s (failure score %.3f)", p.SuggestedTier, t, p.Score))
	}
	rec.FeatureSchema = classify.FeatureSchema
	rec.ClassifyFeatures = append([]float64(nil), f[:]...)
}
