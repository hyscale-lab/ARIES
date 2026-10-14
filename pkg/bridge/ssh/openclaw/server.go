package openclaw

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
)

// prepareWorkspaceScript creates only the alias required by the native SSH
// backend. Existing task files and symlinks are never replaced. The alias stays
// with the independently owned sandbox, including after bridge revocation.
const prepareWorkspaceScript = `set -eu
canonical=$(cd "$1" && pwd -P)
alias="$2"
if [ -e "$alias" ] || [ -L "$alias" ]; then
  echo "OpenClaw workspace path already exists: $alias" >&2
  exit 1
fi
parent=${alias%/*}
relative=${parent#/}
current=
while [ -n "$relative" ]; do
  part=${relative%%/*}
  if [ "$part" = "$relative" ]; then relative=; else relative=${relative#*/}; fi
  current="$current/$part"
  if [ -L "$current" ]; then
    echo "OpenClaw workspace ancestor is a symlink: $current" >&2
    exit 1
  fi
  if [ -e "$current" ]; then
    [ -d "$current" ] || { echo "OpenClaw workspace ancestor is not a directory: $current" >&2; exit 1; }
  else
    mkdir -m 755 -- "$current"
  fi
done
ln -s -- "$canonical" "$alias"
printf '%s\n' "$canonical"`

// Server prepares OpenClaw's filesystem namespace before exposing SSH access.
// It borrows execution and leaves filesystem cleanup to sandbox removal.
type Server struct {
	manager *ssh.Manager
	dialect *Dialect
	mu      sync.Mutex
	used    bool
}

func New(options ssh.Options) (*Server, error) {
	dialect := &Dialect{}
	options.Dialect = dialect
	manager, err := ssh.New(options)
	if err != nil {
		return nil, err
	}
	return &Server{manager: manager, dialect: dialect}, nil
}

func (server *Server) StartTarget(ctx context.Context, sandbox target.Executor) (core.ToolEndpoint, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.used {
		return core.ToolEndpoint{}, errors.New("OpenClaw bridge is already used or revoked")
	}
	server.used = true
	if sandbox == nil {
		return core.ToolEndpoint{}, errors.New("OpenClaw bridge target is required")
	}
	var stdout, stderr bytes.Buffer
	result, err := sandbox.ExecStream(ctx, core.Command{
		Path: remoteShell, Args: []string{"-c", prepareWorkspaceScript, "aries-openclaw-workspace", sandbox.Workdir(), virtualWorkspace},
		Dir: "/", User: "0:0",
	}, nil, &stdout, &stderr)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("prepare OpenClaw workspace alias: %w", err)
	}
	if result.ExitCode != 0 {
		return core.ToolEndpoint{}, fmt.Errorf("prepare OpenClaw workspace alias: exit %d: %s", result.ExitCode, strings.TrimSpace(stderr.String()))
	}
	server.dialect.validationRoot = strings.TrimSuffix(stdout.String(), "\n")
	if !strings.HasPrefix(server.dialect.validationRoot, "/") {
		return core.ToolEndpoint{}, errors.New("OpenClaw workspace setup returned no absolute directory")
	}
	return server.manager.StartTarget(ctx, sandbox)
}

func (server *Server) Stop(ctx context.Context) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.used = true
	return server.manager.Stop(ctx)
}
