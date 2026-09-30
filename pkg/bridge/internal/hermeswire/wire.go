// Package hermeswire is the Hermes wire policy shared by the SSH and gRPC
// bridges: which payloads a Hermes remote may send, and the sandbox command
// each one becomes. It stays on the server side of both bridges because the
// allowlist is a policy gate: the harness container holds the client
// credentials, so a check that ran there could be routed around.
package hermeswire

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Hermes drives its remote through OpenSSH (or a client reproducing it), so
// the wire command is whatever `tools/environments/ssh.py` appends to its
// argv, joined by single spaces. Only four payload shapes are ever produced:
//
//	echo 'SSH connection established'   (_establish_connection, must succeed)
//	echo $HOME                          (_detect_remote_home, failure tolerated)
//	bash -c <shlex-quoted script>       (_run_bash)
//	bash -l -c <shlex-quoted script>    (_run_bash login=True, session snapshot)
//
// Everything else Hermes can emit belongs to its `~/.hermes` file sync
// (mkdir -p, tar xf -, tar cf -, rm -f). ARIES refuses those: the remote is the
// exact container the verifier later inspects, and the sync payload is built
// from `iter_sync_files`, which includes credential files. Refusal is safe —
// Hermes catches and rolls back every sync failure.
const (
	remoteShell = "bash"
	remoteEcho  = "echo"

	// remoteShellPath is where the bare `bash` token resolves. The sandbox
	// requires an absolute command path and performs no PATH lookup, so a
	// Hermes task image must provide /bin/bash.
	remoteShellPath = "/bin/bash"
	bootstrapShell  = "/bin/sh"

	connectionProbePayload = "echo 'SSH connection established'"
	remoteHomePayload      = "echo $HOME"
)

// The kinds classify each request in the evidence record: a well-formed agent
// command, a bootstrap probe, a refused file sync, or a payload that never
// decoded far enough to tell.
const (
	kindAgent     = "agent"
	kindBootstrap = "bootstrap"
	kindSync      = "sync"
	KindUnknown   = "unknown"
)

type remoteCommand struct {
	argv   []string
	script string
	kind   string
}

// syncPayloadPrefixes are the file-sync shapes refused by policy. They are
// matched only to produce an accurate evidence record; an unmatched payload is
// refused just the same.
var syncPayloadPrefixes = []string{"mkdir -p ", "tar xf ", "tar cf ", "rm -f ", "scp "}

// ErrSyncDenied marks a refusal that is policy rather than malformed input.
// Each bridge records its own message for it.
var ErrSyncDenied = errors.New("file sync is denied by ARIES policy")

func isSyncPayload(encoded string) bool {
	for _, prefix := range syncPayloadPrefixes {
		if strings.HasPrefix(encoded, prefix) {
			return true
		}
	}
	return false
}

// Prepare decodes one wire payload and maps it to the command the sandbox
// runs in workdir. The kind classifies the payload even on failure: "sync"
// with ErrSyncDenied, KindUnknown when it does not decode, and the decoded
// kind when only the workdir is refused.
func Prepare(payload, workdir string) (core.Command, string, error) {
	remote, err := decodeRemoteCommand(payload)
	if errors.Is(err, ErrSyncDenied) {
		return core.Command{}, kindSync, err
	}
	if err != nil {
		return core.Command{}, KindUnknown, err
	}
	if !validWorkdir(workdir) {
		return core.Command{}, remote.kind, fmt.Errorf("sandbox workdir %q is not shell-neutral", workdir)
	}
	if remote.kind == kindBootstrap {
		// Replay the literal probe through a POSIX shell so `echo $HOME`
		// reports the sandbox's own home rather than a value ARIES invents.
		return core.Command{Path: bootstrapShell, Args: []string{"-c", payload}, Dir: workdir}, remote.kind, nil
	}
	return core.Command{Path: remoteShellPath, Args: remote.argv[1:], Dir: workdir}, remote.kind, nil
}

// EncodeRemoteCommand is the payload Hermes sends for script: the inverse of decoding an
// agent command. Decoding accepts only this canonical form, so a bridge may
// record an accepted payload verbatim.
func EncodeRemoteCommand(script string, login bool) string {
	if login {
		return remoteShell + " -l -c " + shlexQuote(script)
	}
	return remoteShell + " -c " + shlexQuote(script)
}

func decodeRemoteCommand(encoded string) (remoteCommand, error) {
	if encoded == "" || strings.ContainsRune(encoded, 0) {
		return remoteCommand{}, errors.New("exec command is empty or contains NUL")
	}
	switch encoded {
	case connectionProbePayload:
		return remoteCommand{argv: []string{remoteEcho, "SSH connection established"}, kind: kindBootstrap}, nil
	case remoteHomePayload:
		return remoteCommand{argv: []string{remoteEcho, "$HOME"}, kind: kindBootstrap}, nil
	}
	if isSyncPayload(encoded) {
		return remoteCommand{}, ErrSyncDenied
	}

	remainder, login := strings.CutPrefix(encoded, remoteShell+" -l -c ")
	if !login {
		var ok bool
		remainder, ok = strings.CutPrefix(encoded, remoteShell+" -c ")
		if !ok {
			return remoteCommand{}, errors.New("exec command must invoke only bash -c or bash -l -c")
		}
	}
	script, err := decodeShellToken(remainder)
	if err != nil {
		return remoteCommand{}, err
	}
	if script == "" {
		return remoteCommand{}, errors.New("exec shell script is empty")
	}
	argv := []string{remoteShell}
	if login {
		argv = append(argv, "-l")
	}
	argv = append(argv, "-c", script)
	return remoteCommand{argv: argv, script: script, kind: kindAgent}, nil
}

// decodeShellToken reverses Python's shlex.quote for exactly one token and
// requires the encoding to be canonical, so no second reading of the payload is
// possible. shlex.quote leaves a token bare when it contains only characters in
// its safe set and otherwise wraps it in single quotes, escaping an embedded
// quote as '"'"'.
func decodeShellToken(encoded string) (string, error) {
	if encoded == "" {
		return "", errors.New("exec command has no script token")
	}
	var decoded string
	if encoded[0] != '\'' {
		if strings.ContainsAny(encoded, " \t\n\r'\"\\") {
			return "", errors.New("exec script token is not a single shlex-quoted argument")
		}
		decoded = encoded
	} else {
		var value strings.Builder
		position := 1
		closed := false
		for position < len(encoded) {
			if encoded[position] != '\'' {
				value.WriteByte(encoded[position])
				position++
				continue
			}
			if strings.HasPrefix(encoded[position:], `'"'"'`) {
				value.WriteByte('\'')
				position += 5
				continue
			}
			position++
			closed = true
			break
		}
		if !closed {
			return "", errors.New("exec command contains an unterminated quote")
		}
		if position != len(encoded) {
			return "", errors.New("exec command carries more than one script token")
		}
		decoded = value.String()
	}
	if shlexQuote(decoded) != encoded {
		return "", errors.New("exec script token is not canonically quoted")
	}
	return decoded, nil
}

// shlexQuote mirrors Python's shlex.quote, whose safe set is the ASCII regex
// [^\w@%+=:,./-]. Keeping the two in lockstep is what makes the canonical
// round-trip check above exact.
func shlexQuote(value string) string {
	if value == "" {
		return "''"
	}
	if !strings.ContainsFunc(value, shlexUnsafe) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func shlexUnsafe(value rune) bool {
	switch {
	case value >= 'a' && value <= 'z', value >= 'A' && value <= 'Z', value >= '0' && value <= '9':
		return false
	case strings.ContainsRune("_@%+=:,./-", value):
		return false
	}
	return true
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
