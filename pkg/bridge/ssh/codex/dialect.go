// Package codex adapts Codex's native remote executor to the shared SSH engine.
//
// Codex 0.157.1 has no SSH of its own. Its environments.toml names a program
// that it spawns and talks exec-server JSON-RPC to over stdio; ARIES points
// that at aries-codex-ssh, which carries the stream over one SSH exec request.
// This dialect accepts only that request and runs the staged native
// exec-server inside the exact task sandbox. It translates nothing: shell and
// filesystem semantics stay native to Codex, and the one stream is the
// session's whole tool traffic.
package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/codex/client"
	"github.com/hyscale-lab/aries/pkg/core"
)

// OperationClass labels the executor session in bridge evidence. The
// structured record counts the RPC bytes; Codex's own rollout carries the
// tool-call semantics.
const OperationClass = "executor"

const stagePrefix = "/.aries-codex-"

// StageDir is the root-level directory holding the staged executor for one
// sandbox. Both the Runner-side staging and the bridge-side dialect derive it
// from the sandbox's unique runtime name, so no extra registration field is
// needed. Keeping it at the root avoids following a task-controlled ancestor.
func StageDir(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	return stagePrefix + hex.EncodeToString(sum[:16])
}

// Dialect owns one sandbox's executor admission. Codex spawns its environment
// program once per run, so exactly one executor session is admitted; a second
// request is refused and recorded rather than silently given a fresh executor.
type Dialect struct {
	stage string

	mu      sync.Mutex
	claimed bool
}

var _ ssh.Dialect = (*Dialect)(nil)

func NewDialect(sandboxID string) *Dialect {
	return &Dialect{stage: StageDir(sandboxID)}
}

func (*Dialect) Policy() ssh.Policy {
	return ssh.Policy{
		UnsupportedRequests:   ssh.RejectAndClose,
		InvalidOperationClass: OperationClass,
		RefusedExitCode:       -1,
	}
}

// Prepare maps the fixed remote command onto the native executor. The command
// user is left to the borrowed target, which applies the task's exec user, so
// native commands run as the task identity exactly as a shell command would.
func (dialect *Dialect) Prepare(encoded, workdir string) (ssh.Prepared, *ssh.Refusal) {
	if encoded != client.RemoteCommand {
		return ssh.Prepared{}, &ssh.Refusal{OperationClass: OperationClass, Status: "rejected", Message: "invalid remote command"}
	}
	dialect.mu.Lock()
	claimed := dialect.claimed
	dialect.claimed = true
	dialect.mu.Unlock()
	if claimed {
		return ssh.Prepared{}, &ssh.Refusal{OperationClass: OperationClass, Status: "rejected", Message: "executor session already claimed"}
	}
	return ssh.Prepared{
		Command: core.Command{
			Path: dialect.stage + "/codex", Args: []string{"exec-server", "--listen", "stdio"},
			Dir: workdir, Env: map[string]string{"CODEX_HOME": dialect.stage + "/home"},
		},
		Action:    ssh.Execute,
		HashInput: encoded, Display: encoded,
		OperationClass: OperationClass, RefusalClass: OperationClass,
	}, nil
}
