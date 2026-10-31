package git

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrCheckoutIncomplete reports a working copy whose index or worktree is
// missing files that HEAD tracks. A partial clone checked out while lazy
// fetching was disabled (GIT_NO_LAZY_FETCH=1 with --filter=blob:none) leaves
// exactly this state: HEAD names the full tree, the index is empty, and any
// later commit records every tracked file as deleted.
type ErrCheckoutIncomplete struct {
	Dir             string
	StagedDeletions int
	MissingFiles    int
	Sample          []string
}

func (e *ErrCheckoutIncomplete) Error() string {
	return fmt.Sprintf("checkout at %s is incomplete: %d tracked file(s) staged as deleted against HEAD, %d tracked file(s) missing from the worktree (e.g. %s)",
		e.Dir, e.StagedDeletions, e.MissingFiles, strings.Join(e.Sample, ", "))
}

// CheckoutIntact reports whether the working copy holds every file HEAD
// tracks: nothing staged as deleted against HEAD and no tracked file missing
// from the worktree. A directory that is not a git work tree, or a repository
// whose HEAD has no commit yet, has nothing to compare and is reported intact.
// An incomplete checkout returns *ErrCheckoutIncomplete.
func (g *Git) CheckoutIntact() error {
	inside, err := g.run("rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		return nil
	}
	if _, err := g.run("rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return nil
	}
	staged, err := g.run("diff", "--cached", "--name-only", "--no-renames", "--diff-filter=D", "-z", "HEAD", "--", ".")
	if err != nil {
		return fmt.Errorf("checking index against HEAD in %s: %w", g.workDir, err)
	}
	missing, err := g.run("ls-files", "--deleted", "-z")
	if err != nil {
		return fmt.Errorf("checking worktree against index in %s: %w", g.workDir, err)
	}
	stagedPaths := splitNUL(staged)
	missingPaths := splitNUL(missing)
	if len(stagedPaths) == 0 && len(missingPaths) == 0 {
		return nil
	}
	sample := append(append([]string{}, stagedPaths...), missingPaths...)
	if len(sample) > 3 {
		sample = sample[:3]
	}
	return &ErrCheckoutIncomplete{
		Dir:             g.workDir,
		StagedDeletions: len(stagedPaths),
		MissingFiles:    len(missingPaths),
		Sample:          sample,
	}
}

// HeadCommit returns the commit HEAD names, or "" when HEAD has no commit yet
// or workDir is not a git work tree.
func (g *Git) HeadCommit() string {
	out, err := g.run("rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// UndoCommitsSince moves the current branch back to base when every commit
// in base..HEAD has exactly the given subject and was committed no earlier
// than notBefore (git records whole seconds, so notBefore is truncated to the
// second). The branch moves only if HEAD still names the commit that was
// checked: the ref update is a compare-and-swap, so a commit landing between
// the check and the move fails it instead of being dropped. The index is then
// reset to base and the worktree is left as it is, so files those commits
// added stay on disk untracked. It returns how many commits were undone.
//
// When HEAD is base, or base is empty, it does nothing. When any commit in the
// range has another subject or an earlier commit time, or HEAD no longer
// descends from base, it changes nothing and returns an error naming the
// commit, so work it does not recognise is never moved off the branch.
func (g *Git) UndoCommitsSince(base, subject string, notBefore time.Time) (int, error) {
	if strings.TrimSpace(base) == "" {
		return 0, nil
	}
	head := g.HeadCommit()
	if head == "" || head == base {
		return 0, nil
	}
	if _, err := g.run("merge-base", "--is-ancestor", base, head); err != nil {
		return 0, fmt.Errorf("HEAD %s in %s no longer descends from %s; leaving the branch as it is", short(head), g.workDir, short(base))
	}
	out, err := g.run("log", "--format=%H%x00%ct%x00%s", base+".."+head)
	if err != nil {
		return 0, fmt.Errorf("listing commits since %s in %s: %w", short(base), g.workDir, err)
	}
	floor := notBefore.Truncate(time.Second).Unix()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines {
		fields := strings.SplitN(line, "\x00", 3)
		if len(fields) != 3 {
			return 0, fmt.Errorf("unexpected git log line %q in %s; leaving the branch as it is", line, g.workDir)
		}
		sha, ctRaw, got := fields[0], fields[1], fields[2]
		ct, err := strconv.ParseInt(ctRaw, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("commit %s in %s has commit time %q: %w; leaving the branch as it is", short(sha), g.workDir, ctRaw, err)
		}
		if got != subject {
			return 0, fmt.Errorf("commit %s in %s (%q) is not a %q commit; leaving the branch as it is", short(sha), g.workDir, got, subject)
		}
		if ct < floor {
			return 0, fmt.Errorf("commit %s in %s (%q) predates %s; leaving the branch as it is", short(sha), g.workDir, got, notBefore.UTC().Format(time.RFC3339))
		}
	}
	if _, err := g.run("update-ref", "-m", "undo "+subject, "HEAD", base, head); err != nil {
		return 0, fmt.Errorf("moving %s back to %s (HEAD changed since it was checked?): %w", g.workDir, short(base), err)
	}
	if _, err := g.run("read-tree", base); err != nil {
		return 0, fmt.Errorf("resetting index in %s to %s after moving the branch: %w", g.workDir, short(base), err)
	}
	// read-tree drops cached stat data; refresh it so unchanged files are not
	// reported as modified. A non-zero exit only lists files that differ.
	_, _ = g.run("update-index", "-q", "--refresh")
	return len(lines), nil
}

func splitNUL(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
