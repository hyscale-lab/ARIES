package hermesgrpc

// Adapted from pkg/bridge/hermesssh/workspace.go. The difference is that
// preparedRemoteCommand carries no `encoded` field: the handler records the
// request's own script field, so there is nothing to re-encode.

import (
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
)

// bootstrapShell replays a probe payload literally. remoteShellPath, where the
// bare `bash` token resolves, is declared in bridge.go beside the other
// container paths.
const bootstrapShell = "/bin/sh"

type preparedRemoteCommand struct {
	command core.Command
	kind    string
}

func prepareRemoteCommand(remote remoteCommand, workdir string) (preparedRemoteCommand, error) {
	if !validWorkdir(workdir) {
		return preparedRemoteCommand{}, fmt.Errorf("Hermes gRPC sandbox workdir %q is not shell-neutral", workdir)
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
			kind:    remote.kind,
		}, nil
	case kindAgent:
		return preparedRemoteCommand{
			command: core.Command{Path: remoteShellPath, Args: append([]string(nil), remote.argv[1:]...), Dir: workdir},
			kind:    remote.kind,
		}, nil
	default:
		return preparedRemoteCommand{}, fmt.Errorf("Hermes gRPC command kind %q is unsupported", remote.kind)
	}
}

// validWorkdir keeps the sandbox workdir free of characters that would change
// meaning once it becomes a process working directory or appears in evidence.
func validWorkdir(value string) bool {
	if value == "/" {
		return true
	}
	if len(value) < 2 || value[0] != '/' || value[len(value)-1] == '/' {
		return false
	}
	for _, component := range strings.Split(value[1:], "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		for _, character := range component {
			if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
				continue
			}
			return false
		}
	}
	return true
}
