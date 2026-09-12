package toolathlon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var renameCheckout = os.Rename

// siteConfigs are the gitignored files Toolathlon's own install step derives
// from checked-in examples. Preprocess imports both modules unconditionally,
// so the checkout is unusable without them. The examples already select
// Docker as the container runtime and carry only placeholder credentials,
// which is exactly what the sandbox-side scripts need: the self-hosted
// application tokens come from each task's own token_key_session override.
var siteConfigs = map[string]string{
	"configs/global_configs.py":    "configs/global_configs_example.py",
	"configs/token_key_session.py": "configs/token_key_session_example.py",
}

// Setup creates an exact shallow detached checkout at root and materializes
// the site configs. An existing root is accepted only when it is already at
// the pinned revision.
func Setup(ctx context.Context, root, repositoryURL, revision string) error {
	if repositoryURL == "" {
		return errors.New("toolathlon repository URL is required")
	}
	if revision == "" {
		return errors.New("toolathlon revision is required")
	}
	root = filepath.Clean(root)
	if root == "." || root == string(filepath.Separator) {
		return fmt.Errorf("unsafe toolathlon setup root %q", root)
	}
	info, err := os.Stat(root)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("toolathlon setup root %q is not a directory", root)
		}
		if err := VerifyRevision(ctx, root, revision); err != nil {
			return err
		}
		return materializeSiteConfigs(root)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect toolathlon setup root %q: %w", root, err)
	}

	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create toolathlon cache parent: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".toolathlon-setup-")
	if err != nil {
		return fmt.Errorf("create temporary toolathlon checkout: %w", err)
	}
	defer os.RemoveAll(temporary)

	commands := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", repositoryURL},
		{"fetch", "--depth=1", "origin", revision},
		{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for _, args := range commands {
		if err := runGit(ctx, temporary, args...); err != nil {
			return err
		}
	}
	if err := VerifyRevision(ctx, temporary, revision); err != nil {
		return err
	}
	if err := materializeSiteConfigs(temporary); err != nil {
		return err
	}
	return installCheckout(ctx, temporary, root, revision)
}

// materializeSiteConfigs copies each example to its gitignored name when the
// latter is absent. Because both names are ignored by the pinned .gitignore,
// VerifyRevision still sees a clean checkout afterwards.
func materializeSiteConfigs(root string) error {
	for target, example := range siteConfigs {
		targetPath := filepath.Join(root, filepath.FromSlash(target))
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(example)))
		if err != nil {
			return fmt.Errorf("read toolathlon example config %q: %w", example, err)
		}
		// The targets are gitignored, so VerifyRevision cannot see them:
		// an existing one is accepted only when it is the example, byte
		// for byte. Anything else would change the pinned benchmark.
		existing, err := os.ReadFile(targetPath)
		switch {
		case err == nil && bytes.Equal(existing, content):
			continue
		case err == nil:
			return fmt.Errorf("toolathlon site config %q differs from its pinned example %q; the checkout must not carry local edits", target, example)
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("inspect toolathlon site config %q: %w", target, err)
		}
		if err := os.WriteFile(targetPath, content, 0o644); err != nil {
			return fmt.Errorf("write toolathlon site config %q: %w", target, err)
		}
	}
	return nil
}

func installCheckout(ctx context.Context, temporary, root, revision string) error {
	if err := renameCheckout(temporary, root); err != nil {
		// Another setup may have atomically installed the same pinned checkout
		// after our initial absence check. Accept only a freshly reverified
		// destination; a wrong, dirty, or partial winner remains an error.
		if verifyErr := VerifyRevision(ctx, root, revision); verifyErr == nil {
			return materializeSiteConfigs(root)
		} else {
			return fmt.Errorf("install toolathlon checkout at %q: %w", root, errors.Join(err, verifyErr))
		}
	}
	return nil
}

// VerifyRevision confirms that root is the exact clean pinned checkout.
// Ignored files (the site configs, caches) do not count as changes.
func VerifyRevision(ctx context.Context, root, revision string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("verify toolathlon checkout %q: %w: %s", root, err, output)
	}
	got := strings.TrimSpace(string(output))
	if got != revision {
		return fmt.Errorf("toolathlon checkout %q is revision %q; want pinned %q", root, got, revision)
	}
	status := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain", "--untracked-files=all")
	output, err = status.CombinedOutput()
	if err != nil {
		return fmt.Errorf("inspect toolathlon checkout %q: %w: %s", root, err, output)
	}
	if len(output) != 0 {
		return fmt.Errorf("toolathlon checkout %q has local changes; the pinned benchmark must be clean", root)
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
