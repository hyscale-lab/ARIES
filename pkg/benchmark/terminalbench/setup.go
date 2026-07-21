package terminalbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Setup creates an exact shallow detached checkout at root. An existing root
// is accepted only when it is already at the pinned revision.
func Setup(ctx context.Context, root string) error {
	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) {
		return fmt.Errorf("unsafe terminalbench setup root %q", root)
	}
	info, err := os.Stat(root)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("terminalbench setup root %q is not a directory", root)
		}
		return VerifyRevision(ctx, root)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect terminalbench setup root %q: %w", root, err)
	}

	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create terminalbench cache parent: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".terminal-bench-2-setup-")
	if err != nil {
		return fmt.Errorf("create temporary terminalbench checkout: %w", err)
	}
	defer os.RemoveAll(temporary)

	commands := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", repositoryURL},
		{"fetch", "--depth=1", "origin", Revision},
		{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for _, args := range commands {
		if err := runGit(ctx, temporary, args...); err != nil {
			return err
		}
	}
	if err := VerifyRevision(ctx, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, root); err != nil {
		return fmt.Errorf("install terminalbench checkout at %q: %w", root, err)
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmdArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, output)
	}
	return nil
}
