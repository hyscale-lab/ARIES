package hermes

import (
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/core"
)

// Hermes has no virtual workspace namespace: it addresses the sandbox with
// ordinary absolute paths, so the sandbox workdir is used verbatim and the
// decoded command runs as-is.
const bootstrapShell = "/bin/sh"

type preparedRemoteCommand struct {
	command core.Command
	encoded string
	kind    string
}

func prepareRemoteCommand(remote remoteCommand, workdir string) (preparedRemoteCommand, error) {
	if !bridgekit.ValidWorkdir(workdir) {
		return preparedRemoteCommand{}, fmt.Errorf("Hermes SSH sandbox workdir %q is not shell-neutral", workdir)
	}
	switch remote.kind {
	case kindBootstrap:
		// Replay the literal probe through a POSIX shell so `echo $HOME`
		// reports the sandbox's own home rather than a value ARIES invents.
		encoded := connectionProbePayload
		if remote.argv[1] == "$HOME" {
			encoded = remoteHomePayload
		}
		return preparedRemoteCommand{
			command: core.Command{Path: bootstrapShell, Args: []string{"-c", encoded}, Dir: workdir},
			encoded: encoded,
			kind:    remote.kind,
		}, nil
	case kindAgent:
		return preparedRemoteCommand{
			command: core.Command{Path: bridgekit.RemoteShellPath, Args: append([]string(nil), remote.argv[1:]...), Dir: workdir},
			encoded: encodeRemoteCommand(remote),
			kind:    remote.kind,
		}, nil
	default:
		return preparedRemoteCommand{}, fmt.Errorf("Hermes SSH command kind %q is unsupported", remote.kind)
	}
}

// encodeRemoteCommand reproduces the exact wire payload from the decoded
// command, so the recorded command hash is taken over a canonical form.
func encodeRemoteCommand(remote remoteCommand) string {
	parts := append([]string(nil), remote.argv[:len(remote.argv)-1]...)
	return strings.Join(parts, " ") + " " + shlexQuote(remote.script)
}
