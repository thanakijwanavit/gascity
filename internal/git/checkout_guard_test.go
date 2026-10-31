package git

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initTreeRepo creates a repo with three tracked files in one commit.
func initTreeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")
	for _, f := range []string{"f1", "f2", "sub/f3"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestCheckoutIntactFullCheckout(t *testing.T) {
	dir := initTreeRepo(t)
	if err := New(dir).CheckoutIntact(); err != nil {
		t.Fatalf("CheckoutIntact on full checkout = %v, want nil", err)
	}
}

func TestCheckoutIntactRefusesEmptyIndex(t *testing.T) {
	dir := initTreeRepo(t)
	// The state a blob:none clone leaves when its checkout could not fetch
	// blobs: HEAD names the full tree, the index holds nothing.
	runGit(t, dir, "read-tree", "--empty")
	err := New(dir).CheckoutIntact()
	var inc *ErrCheckoutIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("CheckoutIntact on empty index = %v, want *ErrCheckoutIncomplete", err)
	}
	if inc.StagedDeletions != 3 {
		t.Fatalf("StagedDeletions = %d, want 3", inc.StagedDeletions)
	}
	if !strings.Contains(err.Error(), "staged as deleted") {
		t.Fatalf("error %q does not name the staged deletions", err)
	}
}

func TestCheckoutIntactRefusesMissingWorktreeFile(t *testing.T) {
	dir := initTreeRepo(t)
	if err := os.Remove(filepath.Join(dir, "sub", "f3")); err != nil {
		t.Fatal(err)
	}
	err := New(dir).CheckoutIntact()
	var inc *ErrCheckoutIncomplete
	if !errors.As(err, &inc) {
		t.Fatalf("CheckoutIntact with a missing file = %v, want *ErrCheckoutIncomplete", err)
	}
	if inc.MissingFiles != 1 || inc.Sample[0] != "sub/f3" {
		t.Fatalf("got MissingFiles=%d Sample=%v, want 1 [sub/f3]", inc.MissingFiles, inc.Sample)
	}
}

func TestCheckoutIntactAllowsModifiedAndUntracked(t *testing.T) {
	dir := initTreeRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "f1"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "new")
	if err := New(dir).CheckoutIntact(); err != nil {
		t.Fatalf("CheckoutIntact with edits and a staged add = %v, want nil", err)
	}
}

func TestCheckoutIntactNotARepoOrUnborn(t *testing.T) {
	if err := New(t.TempDir()).CheckoutIntact(); err != nil {
		t.Fatalf("CheckoutIntact outside a repo = %v, want nil", err)
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	if err := New(dir).CheckoutIntact(); err != nil {
		t.Fatalf("CheckoutIntact on unborn HEAD = %v, want nil", err)
	}
	if got := New(dir).HeadCommit(); got != "" {
		t.Fatalf("HeadCommit on unborn HEAD = %q, want empty", got)
	}
}

const bdInitSubject = "bd init: initialize beads issue tracking"

// simulateBdInitCommit reproduces bd init's auto-commit over a gutted index:
// it adds .beads and commits, recording every other tracked file as deleted.
func simulateBdInitCommit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".beads")
	runGit(t, dir, "commit", "-q", "-m", bdInitSubject)
}

func TestUndoCommitsSinceUndoesBdInitCommitAndKeepsFiles(t *testing.T) {
	dir := initTreeRepo(t)
	g := New(dir)
	start := time.Now()
	base := g.HeadCommit()
	runGit(t, dir, "read-tree", "--empty")
	simulateBdInitCommit(t, dir)
	if del := runGit(t, dir, "diff-tree", "-r", "--name-status", "--no-commit-id", "HEAD~1", "HEAD"); strings.Count(del, "D\t") != 3 {
		t.Fatalf("precondition: bd init commit should delete 3 files, got:\n%s", del)
	}

	n, err := g.UndoCommitsSince(base, bdInitSubject, start)
	if err != nil || n != 1 {
		t.Fatalf("UndoCommitsSince = (%d, %v), want (1, nil)", n, err)
	}
	if got := g.HeadCommit(); got != base {
		t.Fatalf("HEAD = %s, want base %s", got, base)
	}
	if err := g.CheckoutIntact(); err != nil {
		t.Fatalf("after undo CheckoutIntact = %v, want nil (index restored to HEAD)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads", "config.yaml")); err != nil {
		t.Fatalf(".beads must stay on disk after undo: %v", err)
	}
}

func TestUndoCommitsSinceLeavesForeignCommits(t *testing.T) {
	dir := initTreeRepo(t)
	g := New(dir)
	start := time.Now()
	base := g.HeadCommit()
	simulateBdInitCommit(t, dir)
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "feat: someone else's work")
	head := g.HeadCommit()

	if n, err := g.UndoCommitsSince(base, bdInitSubject, start); err == nil || n != 0 {
		t.Fatalf("UndoCommitsSince over a foreign commit = (%d, %v), want (0, error)", n, err)
	}
	if got := g.HeadCommit(); got != head {
		t.Fatalf("HEAD moved to %s, want it left at %s", got, head)
	}
}

func TestUndoCommitsSinceNoOps(t *testing.T) {
	dir := initTreeRepo(t)
	g := New(dir)
	start := time.Now()
	base := g.HeadCommit()
	if n, err := g.UndoCommitsSince(base, bdInitSubject, start); err != nil || n != 0 {
		t.Fatalf("UndoCommitsSince at base = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := g.UndoCommitsSince("", bdInitSubject, start); err != nil || n != 0 {
		t.Fatalf("UndoCommitsSince with empty base = (%d, %v), want (0, nil)", n, err)
	}
}

func TestUndoCommitsSinceRefusesDivergedHead(t *testing.T) {
	dir := initTreeRepo(t)
	g := New(dir)
	start := time.Now()
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", bdInitSubject)
	base := g.HeadCommit()
	runGit(t, dir, "reset", "-q", "--hard", "HEAD~1")
	if err := os.WriteFile(filepath.Join(dir, "f1"), []byte("diverged"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "commit", "-q", "-am", bdInitSubject)
	head := g.HeadCommit()
	if _, err := g.UndoCommitsSince(base, bdInitSubject, start); err == nil {
		t.Fatal("UndoCommitsSince with HEAD not descending from base = nil error, want error")
	}
	if got := g.HeadCommit(); got != head {
		t.Fatalf("HEAD moved to %s, want %s", got, head)
	}
}

func TestUndoCommitsSinceRefusesLookalikeSubject(t *testing.T) {
	dir := initTreeRepo(t)
	g := New(dir)
	start := time.Now()
	base := g.HeadCommit()
	runGit(t, dir, "commit", "-q", "--allow-empty", "-m", "bd init: my own follow-up")
	head := g.HeadCommit()
	if _, err := g.UndoCommitsSince(base, bdInitSubject, start); err == nil {
		t.Fatal("UndoCommitsSince over a look-alike subject = nil error, want error")
	}
	if got := g.HeadCommit(); got != head {
		t.Fatalf("HEAD moved to %s, want %s", got, head)
	}
}

func TestUndoCommitsSinceRefusesCommitOlderThanStart(t *testing.T) {
	dir := initTreeRepo(t)
	g := New(dir)
	base := g.HeadCommit()
	cmd := exec.Command("git", "commit", "-q", "--allow-empty", "-m", bdInitSubject)
	cmd.Dir = dir
	cmd.Env = append(sanitizeGitEnv(os.Environ()), "GIT_COMMITTER_DATE=2020-01-01T00:00:00Z")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	head := g.HeadCommit()
	if _, err := g.UndoCommitsSince(base, bdInitSubject, time.Now()); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("UndoCommitsSince over a commit older than start = %v, want a predates error", err)
	}
	if got := g.HeadCommit(); got != head {
		t.Fatalf("HEAD moved to %s, want %s", got, head)
	}
}
