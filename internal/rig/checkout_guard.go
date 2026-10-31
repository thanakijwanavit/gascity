package rig

import (
	"bytes"
	"fmt"
	"os/exec"
)

// validateCheckout stops provisioning before bd can operate on an incomplete index.
func validateCheckout(path string) error {
	git := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = path
		return cmd.Output()
	}
	// A new repository has no committed tree to compare yet.
	if _, err := git("rev-parse", "--verify", "HEAD"); err != nil {
		if _, branchErr := git("symbolic-ref", "-q", "HEAD"); branchErr == nil {
			if refs, refErr := git("for-each-ref", "--format=%(refname)"); refErr == nil && len(refs) == 0 {
				return nil
			}
		}
		return fmt.Errorf("cannot inspect rig checkout HEAD: %w", err)
	}
	index, err := git("ls-files", "-z")
	if err != nil {
		return fmt.Errorf("cannot inspect rig checkout index: %w", err)
	}
	tree, err := git("ls-tree", "-rz", "--name-only", "HEAD")
	if err != nil {
		return fmt.Errorf("cannot inspect rig checkout tree: %w", err)
	}
	missing, err := git("ls-files", "--deleted", "-z")
	if err != nil {
		return fmt.Errorf("cannot inspect rig checkout files: %w", err)
	}
	if bytes.Count(index, []byte{0}) != bytes.Count(tree, []byte{0}) || len(missing) != 0 {
		return fmt.Errorf("incomplete rig checkout: restore the index and files with GIT_NO_LAZY_FETCH unset before adding the rig")
	}
	return nil
}
