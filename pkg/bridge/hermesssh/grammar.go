package hermesssh

import (
	"errors"
	"fmt"
	"strings"
)

// Hermes drives its remote through OpenSSH directly, so the wire command is
// whatever `tools/environments/ssh.py` appends to its argv, joined by single
// spaces. Only four payload shapes are ever produced:
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
// `FileSyncManager.sync` catches every exception, rolls its state back, and
// continues, and `sync_back` then self-suppresses because nothing was pushed.
const (
	remoteShell = "bash"
	remoteEcho  = "echo"

	connectionProbePayload = "echo 'SSH connection established'"
	remoteHomePayload      = "echo $HOME"
)

// classKind separates a well-formed agent command from the bootstrap probes and
// from a refused file-sync attempt, so evidence records say which one occurred.
const (
	kindAgent     = "agent"
	kindBootstrap = "bootstrap"
)

type remoteCommand struct {
	argv   []string
	script string
	login  bool
	kind   string
}

// syncPayloadPrefixes are the file-sync shapes refused by policy. They are
// matched only to produce an accurate evidence record; an unmatched payload is
// refused just the same.
var syncPayloadPrefixes = []string{"mkdir -p ", "tar xf ", "tar cf ", "rm -f ", "scp "}

// errSyncDenied marks a refusal that is policy rather than malformed input.
var errSyncDenied = errors.New("Hermes SSH file sync is denied by ARIES policy")

func isSyncPayload(encoded string) bool {
	for _, prefix := range syncPayloadPrefixes {
		if strings.HasPrefix(encoded, prefix) {
			return true
		}
	}
	return false
}

func decodeRemoteCommand(encoded string) (remoteCommand, error) {
	if encoded == "" || strings.ContainsRune(encoded, 0) {
		return remoteCommand{}, errors.New("SSH exec command is empty or contains NUL")
	}
	switch encoded {
	case connectionProbePayload:
		return remoteCommand{argv: []string{remoteEcho, "SSH connection established"}, kind: kindBootstrap}, nil
	case remoteHomePayload:
		return remoteCommand{argv: []string{remoteEcho, "$HOME"}, kind: kindBootstrap}, nil
	}
	if isSyncPayload(encoded) {
		return remoteCommand{}, errSyncDenied
	}

	remainder, login := strings.CutPrefix(encoded, remoteShell+" -l -c ")
	if !login {
		var ok bool
		remainder, ok = strings.CutPrefix(encoded, remoteShell+" -c ")
		if !ok {
			return remoteCommand{}, errors.New("SSH exec command must invoke only bash -c or bash -l -c")
		}
	}
	script, err := decodeShellToken(remainder)
	if err != nil {
		return remoteCommand{}, err
	}
	if script == "" {
		return remoteCommand{}, errors.New("SSH exec shell script is empty")
	}
	argv := []string{remoteShell}
	if login {
		argv = append(argv, "-l")
	}
	argv = append(argv, "-c", script)
	return remoteCommand{argv: argv, script: script, login: login, kind: kindAgent}, nil
}

// decodeShellToken reverses Python's shlex.quote for exactly one token and
// requires the encoding to be canonical, so no second reading of the payload is
// possible. shlex.quote leaves a token bare when it contains only characters in
// its safe set and otherwise wraps it in single quotes, escaping an embedded
// quote as '"'"'.
func decodeShellToken(encoded string) (string, error) {
	if encoded == "" {
		return "", errors.New("SSH exec command has no script token")
	}
	var decoded string
	if encoded[0] != '\'' {
		if strings.ContainsAny(encoded, " \t\n\r'\"\\") {
			return "", errors.New("SSH exec script token is not a single shlex-quoted argument")
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
			return "", errors.New("SSH exec command contains an unterminated quote")
		}
		if position != len(encoded) {
			return "", errors.New("SSH exec command carries more than one script token")
		}
		decoded = value.String()
	}
	if shlexQuote(decoded) != encoded {
		return "", fmt.Errorf("SSH exec script token is not canonically quoted")
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
