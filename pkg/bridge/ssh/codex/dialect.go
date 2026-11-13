// Package codex adapts Codex's native remote executor to the shared SSH engine.
//
// Codex 0.157.1 has no SSH of its own. Its environments.toml names a program
// that it spawns and talks exec-server JSON-RPC to over stdio; ARIES points
// that at aries-codex-ssh in the harness container, which opens one SSH exec
// per call. Each exec runs a fresh native `codex exec-server --listen stdio`
// staged in the exact task sandbox, so every call is one bridge record, as
// for the other harnesses. The dialect accepts only that fixed command and
// translates nothing: shell, process, and filesystem semantics stay native.
package codex

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/core"
)

// RemoteCommand is the only exec request the dialect accepts. The harness-side
// executor sends exactly this value; wiring tests keep the two in step.
const RemoteCommand = "aries-codex-exec-server-v1"

// OperationClass labels executor calls in bridge evidence. The record's stdin
// holds the call's JSON-RPC request; Codex's rollout holds tool semantics.
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

// Dialect maps the fixed command onto one sandbox's staged executor.
type Dialect struct{ stage string }

var _ ssh.Dialect = Dialect{}

func NewDialect(sandboxID string) Dialect { return Dialect{stage: StageDir(sandboxID)} }

func (Dialect) Policy() ssh.Policy {
	return ssh.Policy{
		UnsupportedRequests:   ssh.RejectAndClose,
		InvalidOperationClass: OperationClass,
		RefusedExitCode:       -1,
	}
}

// Prepare leaves the command user to the borrowed target, which applies the
// task's exec user, so native commands run as the task identity exactly as a
// shell command would.
func (dialect Dialect) Prepare(encoded, workdir string) (ssh.Prepared, *ssh.Refusal) {
	if encoded != RemoteCommand {
		return ssh.Prepared{}, &ssh.Refusal{OperationClass: OperationClass, Status: "rejected", Message: "invalid remote command"}
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
