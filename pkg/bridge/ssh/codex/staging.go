package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// Staging places the pinned static Codex CLI into the live sandbox before the
// bridge exposes access and removes it after release. The Runner-side session
// owns the ordering; the bridge container only executes what is staged.
type Staging struct{ codexPath string }

// NewStaging validates the host-local executable without running it.
func NewStaging(codexPath string) (*Staging, error) {
	resolved, err := filepath.Abs(codexPath)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("Codex executable must be a regular executable file")
	}
	return &Staging{codexPath: resolved}, nil
}

// Stage creates the exclusive root-level directory, uploads the executor, and
// gives the task user a writable CODEX_HOME beside a read-only binary. An
// existing path fails closed instead of being reused.
func (staging *Staging) Stage(ctx context.Context, sandbox runner.Sandbox, target core.BridgeTarget) error {
	stage := StageDir(target.SandboxID)
	if err := run(ctx, sandbox, "create exclusive Codex executor directory", core.Command{Path: "/bin/mkdir", Args: []string{"-m", "0755", "--", stage}}); err != nil {
		return err
	}
	if err := sandbox.Upload(ctx, staging.codexPath, stage+"/codex"); err != nil {
		return fmt.Errorf("stage native Codex executor: %w", err)
	}
	return run(ctx, sandbox, "prepare Codex executor directory", core.Command{
		Path: "/bin/sh", Args: []string{"-c", `mkdir -m 0777 -- "$1/home" && chmod 0777 -- "$1/home" && chmod 0555 -- "$1" "$1/codex"`, "aries-codex-stage", stage},
	})
}

// Remove deletes the staged directory and confirms its absence, so no
// executor staging remains when independent evaluation begins.
func (*Staging) Remove(ctx context.Context, sandbox runner.Sandbox, target core.BridgeTarget) error {
	stage := StageDir(target.SandboxID)
	return run(ctx, sandbox, "confirm Codex executor directory removal", core.Command{
		Path: "/bin/sh", Args: []string{"-c", `rm -rf -- "$1" && test ! -e "$1" && test ! -L "$1"`, "aries-codex-cleanup", stage},
	})
}

func run(ctx context.Context, sandbox runner.Sandbox, step string, command core.Command) error {
	command.User = "0:0"
	result, err := sandbox.Exec(ctx, command)
	if err != nil {
		return fmt.Errorf("%s: %w", step, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%s: status %d", step, result.ExitCode)
	}
	return nil
}
