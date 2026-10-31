package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// makeTrackedRig creates a git rig with three tracked files in one commit.
func makeTrackedRig(t *testing.T) string {
	t.Helper()
	rigPath := filepath.Join(t.TempDir(), "tracked-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitInTest(t, rigPath, "init", "-q")
	runGitInTest(t, rigPath, "config", "user.email", "test@test.com")
	runGitInTest(t, rigPath, "config", "user.name", "Test")
	for _, f := range []string{"f1", "f2", "sub/f3"} {
		if err := os.WriteFile(filepath.Join(rigPath, f), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitInTest(t, rigPath, "add", ".")
	runGitInTest(t, rigPath, "commit", "-q", "-m", "init")
	return rigPath
}

// stubRigInit replaces the rig store init with fn for the test's duration.
func stubRigInit(t *testing.T, cityPath string, fn func(dir string)) *int {
	t.Helper()
	provider := filepath.Join(cityPath, "gc-beads-bd")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "exec:"+provider)
	t.Setenv("GC_DOLT", "")
	origEnsure := initDirIfReadyEnsureBeadsProvider
	origInit := initDirIfReadyInitAndHookDir
	origWait := initDirIfReadyWaitForManagedDolt
	t.Cleanup(func() {
		initDirIfReadyEnsureBeadsProvider = origEnsure
		initDirIfReadyInitAndHookDir = origInit
		initDirIfReadyWaitForManagedDolt = origWait
	})
	initDirIfReadyEnsureBeadsProvider = func(_ string) error { return nil }
	initDirIfReadyWaitForManagedDolt = func(_ string, _ time.Duration) error { return nil }
	calls := new(int)
	initDirIfReadyInitAndHookDir = func(_, dir, _ string) error {
		*calls++
		fn(dir)
		return nil
	}
	return calls
}

// rigGuardGitOut runs git in dir and returns its trimmed stdout.
func rigGuardGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	return rigGuardGitOut(t, dir, "rev-parse", "HEAD")
}

// A checkout whose index lost the files HEAD tracks (a blob:none clone made
// with lazy fetch off) is refused before any store init or commit (g1-zjd31).
func TestDoRigAdd_RefusesGuttedCheckout(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	calls := stubRigInit(t, cityPath, func(string) {})

	rigPath := makeTrackedRig(t)
	head := gitHead(t, rigPath)
	runGitInTest(t, rigPath, "read-tree", "--empty")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "tr", "", false, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRigAdd on gutted checkout = 0, want non-zero; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "is incomplete") || !strings.Contains(stderr.String(), "GIT_NO_LAZY_FETCH") {
		t.Errorf("stderr should explain the gutted checkout and the restore command:\n%s", stderr.String())
	}
	if *calls != 0 {
		t.Errorf("store init ran %d time(s) on a refused rig, want 0", *calls)
	}
	if got := gitHead(t, rigPath); got != head {
		t.Errorf("HEAD moved to %s, want %s", got, head)
	}
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "tracked-rig") {
		t.Errorf("city.toml must not list a refused rig:\n%s", data)
	}
}

// bd init commits onto the rig's branch; rig add moves the branch back and
// leaves the new .beads files on disk.
func TestDoRigAdd_UndoesBdInitCommitOnRigBranch(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	rigPath := makeTrackedRig(t)
	head := gitHead(t, rigPath)

	calls := stubRigInit(t, cityPath, func(dir string) {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "init-marker"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitInTest(t, dir, "add", "-f", ".beads/init-marker")
		runGitInTest(t, dir, "commit", "-q", "-m", "bd init: initialize beads issue tracking")
	})

	var stdout, stderr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "tr", "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigAdd = %d, stderr=%s", code, stderr.String())
	}
	if *calls == 0 {
		t.Fatalf("store init stub never ran; stderr=%s", stderr.String())
	}
	if got := gitHead(t, rigPath); got != head {
		t.Fatalf("HEAD = %s after rig add, want the pre-add commit %s; stderr=%s", got, head, stderr.String())
	}
	if !strings.Contains(stderr.String(), "undid 1 bd init commit") {
		t.Errorf("stderr should report the undone commit:\n%s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "init-marker")); err != nil {
		t.Errorf(".beads files must stay on disk: %v", err)
	}
	for _, f := range []string{"f1", "f2", "sub/f3"} {
		if _, err := os.Stat(filepath.Join(rigPath, f)); err != nil {
			t.Errorf("tracked file %s missing after rig add: %v", f, err)
		}
	}
}

// A commit rig add did not recognise as bd init's is left on the branch.
func TestDoRigAdd_KeepsForeignCommitMadeDuringInit(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")
	rigPath := makeTrackedRig(t)

	stubRigInit(t, cityPath, func(dir string) {
		runGitInTest(t, dir, "commit", "-q", "--allow-empty", "-m", "feat: concurrent work")
	})

	var stdout, stderr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "tr", "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doRigAdd = %d, stderr=%s", code, stderr.String())
	}
	subject := rigGuardGitOut(t, rigPath, "log", "-1", "--format=%s")
	if subject != "feat: concurrent work" {
		t.Fatalf("HEAD subject = %q, want the foreign commit kept", subject)
	}
	if !strings.Contains(stderr.String(), "leaving the branch as it is") {
		t.Errorf("stderr should warn that the branch was left alone:\n%s", stderr.String())
	}
}
