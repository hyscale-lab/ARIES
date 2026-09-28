package roadmapbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Setup installs a shallow pinned checkout containing task descriptions,
// Dockerfiles, and private verifiers. Vendored repositories and solutions are
// excluded from the working tree and their blobs are not fetched.
func Setup(ctx context.Context, root, repositoryURL, revision string) error {
	if strings.TrimSpace(repositoryURL) == "" {
		return errors.New("roadmapbench repository URL is required")
	}
	if strings.TrimSpace(revision) == "" {
		return errors.New("roadmapbench revision is required")
	}
	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) {
		return fmt.Errorf("unsafe roadmapbench setup root %q", root)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	info, err := os.Lstat(root)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("roadmapbench setup root %q is not a directory", root)
		}
		return VerifyRevision(ctx, root, revision)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect roadmapbench setup root %q: %w", root, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create roadmapbench cache parent: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".roadmapbench-setup-")
	if err != nil {
		return fmt.Errorf("create temporary roadmapbench checkout: %w", err)
	}
	defer os.RemoveAll(temporary)

	commands := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", repositoryURL},
		{"config", "remote.origin.promisor", "true"},
		{"config", "remote.origin.partialclonefilter", "blob:none"},
		{"fetch", "--filter=blob:none", "--depth=1", "origin", revision},
		{"sparse-checkout", "init", "--no-cone"},
	}
	for _, args := range commands {
		if _, err := runGit(ctx, temporary, args...); err != nil {
			return err
		}
	}
	output, err := runGit(ctx, temporary, "ls-tree", "-z", "-d", "--name-only", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if len(output) == 0 {
		return errors.New("roadmapbench checkout has no task directories")
	}
	taskIDs := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	for _, id := range taskIDs {
		if !safeTaskID(id) {
			return fmt.Errorf("invalid roadmapbench task directory %q", id)
		}
	}
	// The public promisor remote fails bulk blob requests. Populate one task
	// at a time while keeping vendored repositories and solutions sparse.
	for index, id := range taskIDs {
		arguments := []string{"sparse-checkout", "add"}
		if index == 0 {
			arguments = []string{"sparse-checkout", "set", "--no-cone"}
		}
		for _, suffix := range []string{"task.toml", "instruction.md", "environment/Dockerfile", "tests/**"} {
			arguments = append(arguments, "/"+id+"/"+suffix)
		}
		if _, err := runGit(ctx, temporary, arguments...); err != nil {
			return err
		}
		if index == 0 {
			if _, err := runGit(ctx, temporary, "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
				return err
			}
		}
	}
	if err := VerifyRevision(ctx, temporary, revision); err != nil {
		return err
	}
	return installCheckout(ctx, temporary, root, revision)
}

func installCheckout(ctx context.Context, temporary, root, revision string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(temporary, root); err != nil {
		// Concurrent setups may race to publish the same checkout. The winner
		// must independently pass the exact revision and clean-tree checks.
		if verifyErr := VerifyRevision(ctx, root, revision); verifyErr != nil {
			return fmt.Errorf("install roadmapbench checkout at %q: %w", root, errors.Join(err, verifyErr))
		}
	}
	return nil
}

// VerifyRevision confirms that root is the exact clean pinned checkout. Git's
// sparse index still tracks all selected runtime and verifier inputs, so edits
// to those files are rejected before loading tasks or injecting verifiers.
func VerifyRevision(ctx context.Context, root, revision string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	output, err := runGit(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("verify roadmapbench checkout %q: %w", root, err)
	}
	if got := strings.TrimSpace(string(output)); got != revision {
		return fmt.Errorf("roadmapbench checkout %q is revision %q; want pinned %q", root, got, revision)
	}
	output, err = runGit(ctx, root, "status", "--porcelain", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return fmt.Errorf("inspect roadmapbench checkout %q: %w", root, err)
	}
	if len(output) != 0 {
		return fmt.Errorf("roadmapbench checkout %q has local changes; the pinned dataset must be clean", root)
	}
	return nil
}

func runGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %v: %w: %s", args, err, output)
	}
	return output, nil
}
