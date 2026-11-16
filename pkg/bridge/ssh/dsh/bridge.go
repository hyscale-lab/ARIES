package dsh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
)

// Manager stages the version-matched, credential-free native helper before
// publishing its SSH listener. It borrows execution only, not sandbox lifecycle.
type Manager struct{ *ssh.Manager }

func (manager *Manager) StartTarget(ctx context.Context, sandbox target.Executor) (core.ToolEndpoint, error) {
	if sandbox == nil {
		return core.ToolEndpoint{}, errors.New("DSH target is required")
	}
	workdir := strings.TrimSuffix(sandbox.Workdir(), "/")
	if workdir == "" || workdir == RuntimeRoot || strings.HasPrefix(RuntimeRoot, workdir+"/") || strings.HasPrefix(workdir, RuntimeRoot+"/") {
		return core.ToolEndpoint{}, errors.New("DSH helper must be outside the task workdir")
	}
	archive, err := os.Open("/opt/aries/dsh-helper.tar.gz")
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("open pinned DSH helper: %w", err)
	}
	defer archive.Close()
	result, err := sandbox.ExecStream(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "umask 077; mkdir " + RuntimeRoot + " && tar -xzf - -C " + RuntimeRoot}, Dir: sandbox.Workdir(), StreamProtocol: true}, archive, io.Discard, io.Discard)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("install native DSH helper: %w", err)
	}
	if result.ExitCode != 0 {
		return core.ToolEndpoint{}, fmt.Errorf("install native DSH helper: exit %d", result.ExitCode)
	}
	result, err = sandbox.ExecStream(ctx, core.Command{Path: RuntimeRoot + "/node", Args: []string{"--version"}, Dir: sandbox.Workdir()}, nil, io.Discard, io.Discard)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("probe DSH helper Node runtime: %w", err)
	}
	if result.ExitCode != 0 {
		return core.ToolEndpoint{}, fmt.Errorf("DSH helper requires a compatible Linux glibc sandbox (Node 24): exit %d", result.ExitCode)
	}
	return manager.Manager.StartTarget(ctx, sandbox)
}
