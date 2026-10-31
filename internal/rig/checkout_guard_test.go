package rig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionRejectsGuttedCheckoutBeforeStoreInit(t *testing.T) {
	deps, req, _ := provisionToWritePhase(t)
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = req.Path
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(req.Path, "tracked"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked")
	run("-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "initial")
	run("read-tree", "--empty")
	called := false
	deps.InitStore = func(string, string, string) (bool, error) { called = true; return false, nil }
	_, _, err := Provision(deps, req)
	if err == nil || !strings.Contains(err.Error(), "checkout") {
		t.Fatalf("expected checkout refusal, got %v", err)
	}
	if called {
		t.Fatal("store init ran on gutted checkout")
	}
}

func TestCheckoutGuardAcceptsCompleteAndRejectsMissingTrackedFile(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "tracked"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked")
	run("-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-qm", "initial")
	if err := validateCheckout(dir); err != nil {
		t.Fatalf("complete checkout rejected: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "tracked")); err != nil {
		t.Fatal(err)
	}
	if err := validateCheckout(dir); err == nil {
		t.Fatal("missing tracked file accepted")
	}
}
