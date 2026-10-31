package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// g1-q3bka: on the live g1 city, villatools/gastown.polecat stopped showing
// poolDesired after a supervisor restart. villatools (and villa-research)
// take their gastown pools from [defaults.rig.imports.gastown] plus the city
// pack's [imports.gastown], while image-to-order and others write their own
// [rigs.imports.gastown] block. The suspicion was that rigs fed by the default
// imports drop out of pool demand. This fixture reproduces that layout from
// real TOML files through config.Load (so the binding-qualified template names
// come from pack expansion, not a hand-built config) and pins that both rig
// shapes count their routed work as pool demand.
func writeDefaultRigImportsCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(cityPath, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Local stand-in for the gastown pack: one rig-scoped, min-0 pool.
	write("packs/gastown/pack.toml", "[pack]\nname = \"gastown\"\nschema = 2\n")
	write("packs/gastown/agents/polecat/agent.toml", `scope = "rig"
start_command = "true"
min_active_sessions = 0
max_active_sessions = 5
`)
	// City pack imports gastown at city level, like g1's vendored no-mayor copy.
	write("pack.toml", `[pack]
name = "test-city"
schema = 2

[imports.gastown]
source = "./packs/gastown"
`)
	write("city.toml", `[workspace]
name = "test-city"

[defaults.rig.imports.gastown]
source = "./packs/gastown"

[[rigs]]
name = "deflt"

[[rigs]]
name = "own"
[rigs.imports]
[rigs.imports.gastown]
source = "./packs/gastown"
`)
	site := "workspace_name = \"test-city\"\n"
	for _, rig := range []string{"deflt", "own"} {
		rigPath := filepath.Join(cityPath, rig)
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		site += "\n[[rig]]\nname = \"" + rig + "\"\npath = \"" + rigPath + "\"\n"
	}
	write(".gc/site.toml", site)
	return cityPath
}

func TestBuildDesiredState_DefaultRigImportsPoolGetsPoolDesired(t *testing.T) {
	cityPath := writeDefaultRigImportsCity(t)
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("config.LoadWithIncludes: %v", err)
	}

	templates := map[string]string{} // rig -> qualified polecat template
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Name == "polecat" && a.Dir != "" {
			templates[a.Dir] = a.QualifiedName()
		}
	}
	for _, rig := range []string{"deflt", "own"} {
		if templates[rig] != rig+"/gastown.polecat" {
			t.Fatalf("rig %q polecat template = %q, want %q (all polecats: %v)", rig, templates[rig], rig+"/gastown.polecat", templates)
		}
	}

	cityStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"deflt": beads.NewMemStore(), "own": beads.NewMemStore()}
	for rig, template := range templates {
		for i := 0; i < 2; i++ {
			if _, err := rigStores[rig].Create(beads.Bead{
				Title:    "routed work",
				Type:     "task",
				Status:   "open",
				Metadata: map[string]string{"gc.routed_to": template},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	ds := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, rigStores, nil, nil, io.Discard,
	)
	for rig, template := range templates {
		if got := ds.ScaleCheckCounts[template]; got != 2 {
			t.Errorf("ScaleCheckCounts[%s] = %d, want 2 (rig %q; counts=%v partial=%v)",
				template, got, rig, ds.ScaleCheckCounts, ds.PoolScaleCheckPartialTemplates)
		}
	}
}

// A dispatch hold is the reason the two ready villatools beads seen on
// 31 Oct 23:00Z (vi-dbad, vi-9buh, both hold:mayor) did not spawn: held work
// must not count as pool demand for a default-imported rig either.
func TestBuildDesiredState_DefaultRigImportsPoolIgnoresHeldWork(t *testing.T) {
	cityPath := writeDefaultRigImportsCity(t)
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("config.LoadWithIncludes: %v", err)
	}
	const template = "deflt/gastown.polecat"
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"deflt": rigStore, "own": beads.NewMemStore()}
	if _, err := rigStore.Create(beads.Bead{
		Title: "held", Type: "task", Status: "open",
		Labels:   []string{"hold:mayor"},
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rigStore.Create(beads.Bead{
		Title: "free", Type: "task", Status: "open",
		Metadata: map[string]string{"gc.routed_to": template},
	}); err != nil {
		t.Fatal(err)
	}
	ds := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		beads.NewMemStore(), rigStores, nil, nil, io.Discard,
	)
	if got := ds.ScaleCheckCounts[template]; got != 1 {
		t.Fatalf("ScaleCheckCounts[%s] = %d, want 1: only the unheld bead is pool demand (counts=%v)", template, got, ds.ScaleCheckCounts)
	}
}
