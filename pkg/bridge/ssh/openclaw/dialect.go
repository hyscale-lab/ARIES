// Package openclaw adapts OpenClaw's native SSH command and workspace dialect.
package openclaw

import (
	"maps"
	"slices"
	"strings"

	"github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/core"
)

// Dialect preserves OpenClaw's pinned workspace controls and quoting grammar.
// It owns no transport or execution lifecycle.
type Dialect struct{}

var _ ssh.Dialect = Dialect{}

func (Dialect) Policy() ssh.Policy {
	return ssh.Policy{
		UnsupportedRequests:   ssh.RejectAndClose,
		InvalidOperationClass: "exec",
	}
}

func (Dialect) Prepare(encoded, workdir string) (ssh.Prepared, *ssh.Refusal) {
	remote, err := decodeRemoteCommand(encoded)
	if err != nil {
		return ssh.Prepared{}, rejected()
	}
	prepared, err := prepareRemoteCommand(remote, workdir)
	if err != nil {
		return ssh.Prepared{}, rejected()
	}
	action := ssh.Execute
	if prepared.suppressed {
		action = ssh.DrainOnly
	}
	return ssh.Prepared{
		Command: prepared.command, Action: action,
		HashInput: prepared.encoded, Display: replayDisplayCommand(prepared.command),
		OperationClass: operationClass(prepared.command), RefusalClass: "exec",
		WorkspaceHome: prepared.workspaceHome,
		Environment:   slices.Sorted(maps.Keys(prepared.command.Env)),
	}, nil
}

func rejected() *ssh.Refusal {
	return &ssh.Refusal{OperationClass: "exec", Status: "rejected", Message: "invalid remote command"}
}

func (remote remoteCommand) command(workdir string) core.Command {
	index := 0
	environment := make(map[string]string)
	if remote.argv[0] == remoteEnv {
		index++
		for remote.argv[index] != remoteShell {
			name, value, _ := strings.Cut(remote.argv[index], "=")
			environment[name] = value
			index++
		}
	}
	if len(environment) == 0 {
		environment = nil
	}
	return core.Command{Path: remote.argv[index], Args: append([]string(nil), remote.argv[index+1:]...), Dir: workdir, Env: environment}
}

func replayDisplayCommand(command core.Command) string {
	if operationClass(command) != "exec" {
		return ""
	}
	if command.Path == remoteShell && len(command.Args) >= 2 && command.Args[0] == "-c" {
		return command.Args[1]
	}
	return command.Path
}

func operationClass(command core.Command) string {
	if command.Path == remoteShell && matchesExactArgv(command.Args, "-c", directoryUploadScript, directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot) {
		return "workspace_upload"
	}
	return "exec"
}
