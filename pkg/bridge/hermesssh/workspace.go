package hermesssh

import (
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Hermes has no virtual workspace namespace. It learns its working directory
// from the rendered `terminal.cwd` and its remote home from `echo $HOME`, then
// addresses the sandbox with ordinary absolute paths, so there is nothing to
// translate. The mapping here is therefore direct: the sandbox workdir is used
// verbatim and the decoded command runs as-is.
const (
	bootstrapShell = "/bin/sh"

	// wireShell is the bare token Hermes puts on the wire; remoteShellPath is
	// where it is resolved to. The sandbox requires an absolute command path
	// and performs no PATH lookup of its own, so the mapping has to happen
	// here. This is why a Hermes task image must provide /bin/bash.
	remoteShellPath = "/bin/bash"
)

type preparedRemoteCommand struct {
	command core.Command
	encoded string
	kind    string
}

func prepareRemoteCommand(remote remoteCommand, workdir string) (preparedRemoteCommand, error) {
	if !validWorkdir(workdir) {
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
			command: core.Command{Path: remoteShellPath, Args: append([]string(nil), remote.argv[1:]...), Dir: workdir},
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
