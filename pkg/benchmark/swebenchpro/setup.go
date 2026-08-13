package swebenchpro

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const gitLFSPointerPrefix = "version https://git-lfs.github.com/spec"

var renameSetupRoot = os.Rename

// Setup installs exact, detached dataset and evaluator checkouts beneath root.
// The pair is prepared privately and published with one rename so callers
// never observe a partially installed benchmark. An existing root is accepted
// only when both checkouts are already clean and at their pinned revisions.
func Setup(ctx context.Context, root, datasetURL, datasetRevision, evaluatorURL, evaluatorRevision string) error {
	if strings.TrimSpace(datasetURL) == "" {
		return errors.New("SWE-bench Pro dataset repository URL is required")
	}
	if strings.TrimSpace(datasetRevision) == "" {
		return errors.New("SWE-bench Pro dataset revision is required")
	}
	if strings.TrimSpace(evaluatorURL) == "" {
		return errors.New("SWE-bench Pro evaluator repository URL is required")
	}
	if strings.TrimSpace(evaluatorRevision) == "" {
		return errors.New("SWE-bench Pro evaluator revision is required")
	}

	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) {
		return fmt.Errorf("unsafe SWE-bench Pro setup root %q", root)
	}
	if info, err := os.Stat(root); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("SWE-bench Pro setup root %q is not a directory", root)
		}
		return verifySetupRoot(ctx, root, datasetRevision, evaluatorRevision)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect SWE-bench Pro setup root %q: %w", root, err)
	}

	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create SWE-bench Pro cache parent: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".swe-bench-pro-setup-")
	if err != nil {
		return fmt.Errorf("create temporary SWE-bench Pro root: %w", err)
	}
	defer os.RemoveAll(temporary)

	datasetRoot := filepath.Join(temporary, "dataset")
	if err := checkoutRevision(ctx, datasetRoot, datasetURL, datasetRevision, true); err != nil {
		return fmt.Errorf("install SWE-bench Pro dataset: %w", err)
	}
	evaluatorRoot := filepath.Join(temporary, "evaluator")
	if err := checkoutRevision(ctx, evaluatorRoot, evaluatorURL, evaluatorRevision, false); err != nil {
		return fmt.Errorf("install SWE-bench Pro evaluator: %w", err)
	}
	if err := verifySetupRoot(ctx, temporary, datasetRevision, evaluatorRevision); err != nil {
		return err
	}
	return installSetupRoot(ctx, temporary, root, datasetRevision, evaluatorRevision)
}

func checkoutRevision(ctx context.Context, root, repositoryURL, revision string, resolveDatasetLFS bool) error {
	if err := os.Mkdir(root, 0o755); err != nil {
		return fmt.Errorf("create checkout %q: %w", root, err)
	}
	commands := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", repositoryURL},
		{"fetch", "--depth=1", "origin", revision},
	}
	for _, args := range commands {
		if err := runSetupGit(ctx, root, false, args...); err != nil {
			return err
		}
	}
	// Avoid relying on checkout-time LFS smudging. Explicit resolution below
	// gives a clear error and lets us positively reject an unresolved pointer.
	if err := runSetupGit(ctx, root, resolveDatasetLFS, "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		return err
	}
	if resolveDatasetLFS {
		pointer, err := datasetParquetIsLFSPointer(root)
		if err != nil {
			return err
		}
		if pointer {
			if err := runSetupGit(ctx, root, false, "lfs", "pull"); err != nil {
				return fmt.Errorf("resolve SWE-bench Pro dataset Git LFS objects: %w", err)
			}
		}
		pointer, err = datasetParquetIsLFSPointer(root)
		if err != nil {
			return err
		}
		if pointer {
			return fmt.Errorf("SWE-bench Pro dataset parquet %q remains a Git LFS pointer", datasetParquetPath)
		}
	}
	return VerifyRevision(ctx, root, revision)
}

func datasetParquetIsLFSPointer(root string) (bool, error) {
	path := filepath.Join(root, datasetParquetPath)
	info, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("inspect SWE-bench Pro dataset parquet %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("SWE-bench Pro dataset parquet %q is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open SWE-bench Pro dataset parquet %q: %w", path, err)
	}
	defer file.Close()
	prefix := make([]byte, len(gitLFSPointerPrefix))
	read, err := io.ReadFull(file, prefix)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, fmt.Errorf("read SWE-bench Pro dataset parquet %q: %w", path, err)
	}
	if read == 0 {
		return false, errors.New("SWE-bench Pro dataset parquet is empty")
	}
	return read == len(prefix) && string(prefix) == gitLFSPointerPrefix, nil
}

func installSetupRoot(ctx context.Context, temporary, root, datasetRevision, evaluatorRevision string) error {
	if err := renameSetupRoot(temporary, root); err != nil {
		// A concurrent setup may have published the same pair. Only an exact,
		// freshly verified winner is accepted; partial or different roots fail.
		if verifyErr := verifySetupRoot(ctx, root, datasetRevision, evaluatorRevision); verifyErr == nil {
			return nil
		} else {
			return fmt.Errorf("install SWE-bench Pro root at %q: %w", root, errors.Join(err, verifyErr))
		}
	}
	return nil
}

func verifySetupRoot(ctx context.Context, root, datasetRevision, evaluatorRevision string) error {
	if err := VerifyRevision(ctx, filepath.Join(root, "dataset"), datasetRevision); err != nil {
		return fmt.Errorf("verify SWE-bench Pro dataset checkout: %w", err)
	}
	pointer, err := datasetParquetIsLFSPointer(filepath.Join(root, "dataset"))
	if err != nil {
		return err
	}
	if pointer {
		return fmt.Errorf("SWE-bench Pro dataset parquet %q is an unresolved Git LFS pointer", datasetParquetPath)
	}
	if err := VerifyRevision(ctx, filepath.Join(root, "evaluator"), evaluatorRevision); err != nil {
		return fmt.Errorf("verify SWE-bench Pro evaluator checkout: %w", err)
	}
	return nil
}

// VerifyRevision confirms that root is the exact clean pinned checkout.
func VerifyRevision(ctx context.Context, root, revision string) error {
	command := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify SWE-bench Pro checkout %q: %w: %s", root, err, output)
	}
	got := strings.TrimSpace(string(output))
	if got != revision {
		return fmt.Errorf("SWE-bench Pro checkout %q is revision %q; want pinned %q", root, got, revision)
	}
	command = exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "--untracked-files=all")
	output, err = command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect SWE-bench Pro checkout %q: %w: %s", root, err, output)
	}
	if len(output) != 0 {
		return fmt.Errorf("SWE-bench Pro checkout %q has local changes; the pinned source must be clean", root)
	}
	return nil
}

func runSetupGit(ctx context.Context, dir string, skipLFSSmudge bool, args ...string) error {
	commandArgs := append([]string{"-C", dir}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	if skipLFSSmudge {
		command.Env = append(os.Environ(), "GIT_LFS_SKIP_SMUDGE=1")
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, output)
	}
	return nil
}
