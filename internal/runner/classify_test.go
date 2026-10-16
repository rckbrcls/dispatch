package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dispatch/internal/adapter"
	"dispatch/internal/classify"
	"dispatch/internal/store"
	"dispatch/internal/tiers"
)

func TestClassifierSensitiveTierAndResume(t *testing.T) {
	var ran []string
	d := deps(t, &ran, fake{name: "a", out: "ok", ran: &ran})
	d.Table.Tiers[adapter.TierNormal] = []tiers.Entry{{Agent: "a", Model: "normal"}}
	d.Table.Tiers[adapter.TierHard] = []tiers.Entry{{Agent: "a", Model: "hard"}}
	for _, requested := range []adapter.Tier{adapter.TierEasy, adapter.TierNormal, adapter.TierHard} {
		r, err := d.Run(context.Background(), Options{Tier: requested, Prompt: "fix auth", ReadOnly: true})
		if err != nil || r.Status != store.Completed || r.Tier != string(classify.Higher(requested)) || r.RequestedTier != string(requested) || r.ExecutionTier != r.Tier {
			t.Fatalf("promotion: %+v %v", r, err)
		}
		persisted, err := d.Store.Get(r.ID)
		if err != nil || persisted.FeatureSchema != classify.FeatureSchema || len(persisted.ClassifyFeatures) != 24 {
			t.Fatalf("snapshot missing: %+v %v", persisted, err)
		}
		resume, err := d.Run(context.Background(), Options{Resume: r.ID, Prompt: "fix auth again", ReadOnly: true})
		if err != nil || resume.Status != store.Completed || resume.Tier != r.Tier || resume.Model != r.Model {
			t.Fatalf("resume promoted session: %+v %v", resume, err)
		}
	}
}

func TestClassifierLearnedWarningDoesNotPromote(t *testing.T) {
	var ran []string
	d := deps(t, &ran, fake{name: "a", out: "ok", ran: &ran})
	f := classify.Extract(classify.Input{Prompt: "create helper", Tier: adapter.TierEasy})
	for range 150 {
		if err := classify.Observe(d.classifierPath(), store.Record{
			Status: store.Completed, Verification: "verification_failed", ExecutionTier: "easy",
			FeatureSchema: classify.FeatureSchema, ClassifyFeatures: f[:],
		}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "create helper", ReadOnly: true})
	if err != nil || r.Status != store.Completed || r.Tier != "easy" || !strings.Contains(r.Note, "classifier warning: suggests tier normal") {
		t.Fatalf("advisory: %+v %v", r, err)
	}
}

func TestClassifierTrainingOnFinishToleratesBadState(t *testing.T) {
	for _, state := range []string{"corrupt", "unwritable"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("DISPATCH_HOME", t.TempDir())
			root := scriptRepo(t, "#!/bin/sh\nexit 0\n")
			var ran []string
			f := editFake{fake: fake{name: "a", out: "ok", ran: &ran}, edit: func(dir string) {
				if err := os.WriteFile(filepath.Join(dir, "created.txt"), []byte("ok\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}}
			d := deps(t, &ran, f)
			if state == "corrupt" {
				if err := os.WriteFile(d.classifierPath(), []byte("{broken"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				// A directory at the model path fails both Load and atomic Rename.
				if err := os.Mkdir(d.classifierPath(), 0700); err != nil {
					t.Fatal(err)
				}
			}
			r, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "create created.txt", Dir: root})
			if err != nil || r.Status != store.Completed || r.Verification != "verified" {
				t.Fatalf("classifier broke run: %+v %v", r, err)
			}
			stored, err := d.Store.Get(r.ID)
			if err != nil || stored.Verification != "verified" {
				t.Fatalf("finish not recorded: %+v %v", stored, err)
			}
			if state == "corrupt" {
				m, err := classify.Load(d.classifierPath())
				if err != nil || m.Tiers[adapter.TierEasy].Samples != 1 {
					t.Fatalf("finish did not train: %+v %v", m, err)
				}
			}
		})
	}
}

func TestClassifierUsesPreRunSnapshotAndExecutedTier(t *testing.T) {
	t.Setenv("DISPATCH_HOME", t.TempDir())
	root := scriptRepo(t, "#!/bin/sh\nexit 0\n")
	var ran []string
	f := editFake{fake: fake{name: "a", out: "ok", ran: &ran}, edit: func(dir string) {
		os.WriteFile(filepath.Join(dir, "new.go"), []byte("package demo\n"), 0600)
	}}
	d := deps(t, &ran, f)
	d.Table.Tiers[adapter.TierNormal] = []tiers.Entry{{Agent: "a", Model: "normal"}}
	r, err := d.Run(context.Background(), Options{Tier: adapter.TierEasy, Prompt: "fix auth in new.go", Dir: root})
	if err != nil || r.Status != store.Completed || r.ExecutionTier != "normal" {
		t.Fatalf("%+v %v", r, err)
	}
	pre := classify.Extract(classify.Input{Prompt: r.Prompt, Dir: root, Tier: adapter.TierNormal})
	// new.go exists only in the agent's worktree; the saved count stays zero.
	for i, x := range pre {
		if r.ClassifyFeatures[i] != x {
			t.Fatalf("feature %d changed during execution", i)
		}
	}
	m, err := classify.Load(d.classifierPath())
	if err != nil || m.Tiers[adapter.TierNormal].Samples != 1 || m.Tiers[adapter.TierEasy].Samples != 0 {
		t.Fatalf("wrong training head: %+v %v", m, err)
	}
}
