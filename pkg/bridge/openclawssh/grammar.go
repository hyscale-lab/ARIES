package openclawssh

import (
	"errors"
	"fmt"
	"strings"
)

const (
	remoteShell = "/bin/sh"
	remoteEnv   = "env"
)

type remoteCommand struct {
	argv []string
}

func decodeRemoteCommand(encoded string) (remoteCommand, error) {
	if encoded == "" || strings.ContainsRune(encoded, 0) {
		return remoteCommand{}, errors.New("SSH exec command is empty or contains NUL")
	}
	tokens, err := decodeCanonicalTokens(encoded)
	if err != nil {
		return remoteCommand{}, err
	}
	if encodeCanonicalTokens(tokens) != encoded {
		return remoteCommand{}, errors.New("SSH exec command is not canonically quoted")
	}

	shellIndex := 0
	if tokens[0] == remoteEnv {
		shellIndex = 1
		seen := make(map[string]struct{})
		sawOpenClawShell := false
		for shellIndex < len(tokens) && tokens[shellIndex] != remoteShell {
			assignment := tokens[shellIndex]
			separator := strings.IndexByte(assignment, '=')
			if separator <= 0 {
				return remoteCommand{}, fmt.Errorf("SSH exec environment assignment %q is malformed", assignment)
			}
			name := assignment[:separator]
			if !validEnvironmentName(name) {
				return remoteCommand{}, fmt.Errorf("SSH exec environment name %q is invalid", name)
			}
			if sawOpenClawShell {
				return remoteCommand{}, errors.New("SSH exec OPENCLAW_SHELL assignment must be last")
			}
			value := assignment[separator+1:]
			if !allowedEnvironmentAssignment(name, value) {
				return remoteCommand{}, fmt.Errorf("SSH exec environment assignment %q is not allowed", assignment)
			}
			if _, exists := seen[name]; exists {
				return remoteCommand{}, fmt.Errorf("SSH exec environment name %q is repeated", name)
			}
			seen[name] = struct{}{}
			sawOpenClawShell = name == "OPENCLAW_SHELL"
			shellIndex++
		}
	}
	if shellIndex+2 >= len(tokens) || tokens[shellIndex] != remoteShell || tokens[shellIndex+1] != "-c" {
		return remoteCommand{}, errors.New("SSH exec command must invoke only /bin/sh -c, optionally through env")
	}
	if tokens[shellIndex+2] == "" {
		return remoteCommand{}, errors.New("SSH exec shell script is empty")
	}
	return remoteCommand{argv: tokens}, nil
}

func decodeCanonicalTokens(encoded string) ([]string, error) {
	var tokens []string
	for position := 0; position < len(encoded); {
		if encoded[position] != '\'' {
			return nil, fmt.Errorf("SSH exec token at byte %d is not single-quoted", position)
		}
		position++
		var value strings.Builder
		closed := false
		for position < len(encoded) {
			if encoded[position] != '\'' {
				value.WriteByte(encoded[position])
				position++
				continue
			}
			if position+4 < len(encoded) && encoded[position] == '\'' && encoded[position+1] == '"' && encoded[position+2] == '\'' && encoded[position+3] == '"' && encoded[position+4] == '\'' {
				value.WriteByte('\'')
				position += 5
				continue
			}
			position++
			closed = true
			break
		}
		if !closed {
			return nil, errors.New("SSH exec command contains an unterminated quote")
		}
		tokens = append(tokens, value.String())
		if position == len(encoded) {
			break
		}
		if encoded[position] != ' ' {
			return nil, fmt.Errorf("SSH exec tokens are not separated by one space at byte %d", position)
		}
		position++
		if position == len(encoded) || encoded[position] == ' ' {
			return nil, errors.New("SSH exec command contains empty token spacing")
		}
	}
	if len(tokens) == 0 {
		return nil, errors.New("SSH exec command has no tokens")
	}
	return tokens, nil
}

func encodeCanonicalTokens(tokens []string) string {
	quoted := make([]string, len(tokens))
	for index, token := range tokens {
		quoted[index] = "'" + strings.ReplaceAll(token, "'", `'"'"'`) + "'"
	}
	return strings.Join(quoted, " ")
}

func validEnvironmentName(name string) bool {
	for index, character := range name {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return name != ""
}

func allowedEnvironmentAssignment(name, value string) bool {
	switch name {
	case "OPENCLAW_SHELL":
		return value == "exec"
	case "PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TMPDIR", "TZ":
		return true
	default:
		return false
	}
}
