package openclawssh

import (
	"errors"
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	virtualRuntimeRoot = "/aries/openclaw/openclaw-ssh-shared-8198076c"
	virtualWorkspace   = virtualRuntimeRoot + "/workspace"

	runtimeProbeScript   = `if [ -d "$1" ]; then printf "1\n"; else printf "0\n"; fi`
	runtimeProbeSentinel = "openclaw-sandbox-check"
	workspaceClearScript = `mkdir -p -- "$1" && find "$1" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +`
	workspaceClearLabel  = "openclaw-sandbox-clear"
	runtimeRemoveScript  = `rm -rf -- "$1"`
	runtimeRemoveLabel   = "openclaw-sandbox-remove"

	generatedWorkspacePrefix = "cd '" + virtualWorkspace + "' && "
)

type preparedRemoteCommand struct {
	command       core.Command
	encoded       string
	workspaceHome string
	suppressed    bool
}

func prepareRemoteCommand(remote remoteCommand, workdir string) (preparedRemoteCommand, error) {
	if !validVirtualizationWorkdir(workdir) {
		return preparedRemoteCommand{}, fmt.Errorf("OpenClaw SSH sandbox workdir %q is not shell-neutral", workdir)
	}
	if matchesExactArgv(remote.argv, remoteShell, "-c", runtimeProbeScript, runtimeProbeSentinel, virtualRuntimeRoot) {
		argv := append([]string(nil), remote.argv...)
		argv[len(argv)-1] = workdir
		return preparedFromArgv(argv, workdir, "", false), nil
	}
	if matchesExactArgv(remote.argv, remoteShell, "-c", workspaceClearScript, workspaceClearLabel, virtualWorkspace) ||
		matchesExactArgv(remote.argv, remoteShell, "-c", runtimeRemoveScript, runtimeRemoveLabel, virtualRuntimeRoot) {
		return preparedFromArgv([]string{remoteShell, "-c", ":"}, workdir, "", true), nil
	}

	shellIndex := remote.shellIndex()
	script := remote.argv[shellIndex+2]
	if !strings.HasPrefix(script, generatedWorkspacePrefix) {
		if remote.hasExactAssignment("HOME", virtualWorkspace) {
			return preparedRemoteCommand{}, errors.New("OpenClaw SSH virtual HOME is outside the generated command shape")
		}
		return preparedFromArgv(remote.argv, workdir, "", false), nil
	}

	argv, err := mapGeneratedHome(remote.argv, shellIndex, workdir)
	if err != nil {
		return preparedRemoteCommand{}, err
	}
	remainder := script[len(generatedWorkspacePrefix):]
	translated, err := translateVirtualWorkspace(remainder, workdir)
	if err != nil {
		return preparedRemoteCommand{}, err
	}
	argv[shellIndex+2] = translated
	return preparedFromArgv(argv, workdir, workdir, false), nil
}

func preparedFromArgv(argv []string, workdir, workspaceHome string, suppressed bool) preparedRemoteCommand {
	remote := remoteCommand{argv: append([]string(nil), argv...)}
	return preparedRemoteCommand{
		command:       remote.command(workdir),
		encoded:       encodeCanonicalTokens(argv),
		workspaceHome: workspaceHome,
		suppressed:    suppressed,
	}
}

func matchesExactArgv(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func mapGeneratedHome(argv []string, shellIndex int, workdir string) ([]string, error) {
	if shellIndex != 5 || len(argv) < 8 || argv[0] != remoteEnv {
		return nil, errors.New("OpenClaw SSH generated environment has an unexpected shape")
	}
	wantNames := [...]string{"PATH", "HOME", "LANG", "OPENCLAW_SHELL"}
	for offset, wantName := range wantNames {
		name, value, found := strings.Cut(argv[offset+1], "=")
		if !found || name != wantName {
			return nil, errors.New("OpenClaw SSH generated environment order is invalid")
		}
		switch wantName {
		case "HOME":
			if value != virtualWorkspace {
				return nil, errors.New("OpenClaw SSH generated HOME does not match the virtual workspace")
			}
		case "OPENCLAW_SHELL":
			if value != "exec" {
				return nil, errors.New("OpenClaw SSH generated shell sentinel is invalid")
			}
		}
	}
	mapped := append([]string(nil), argv...)
	mapped[2] = "HOME=" + workdir
	return mapped, nil
}

func translateVirtualWorkspace(command, workdir string) (string, error) {
	var output strings.Builder
	for position := 0; position < len(command); {
		workspaceOffset := strings.Index(command[position:], virtualWorkspace)
		runtimeOffset := strings.Index(command[position:], virtualRuntimeRoot)
		if workspaceOffset < 0 && runtimeOffset < 0 {
			output.WriteString(command[position:])
			break
		}
		if runtimeOffset >= 0 && (workspaceOffset < 0 || runtimeOffset < workspaceOffset) {
			return "", errors.New("OpenClaw SSH ordinary command contains the virtual runtime root")
		}
		match := position + workspaceOffset
		output.WriteString(command[position:match])
		end := match + len(virtualWorkspace)
		if !validWorkspaceLeftBoundary(command, match) || !validWorkspaceRightBoundary(command, end) {
			return "", errors.New("OpenClaw SSH command contains an ambiguous virtual workspace reference")
		}
		if end+1 < len(command) && command[end] == '/' && command[end+1] == '/' {
			return "", errors.New("OpenClaw SSH command contains an ambiguous repeated-slash workspace descendant")
		}
		if workdir == "/" && end < len(command) && command[end] == '/' {
			// The untouched descendant suffix already begins at container root.
		} else {
			output.WriteString(workdir)
		}
		position = end
	}
	translated := output.String()
	if strings.Contains(translated, virtualWorkspace) || strings.Contains(translated, virtualRuntimeRoot) {
		return "", errors.New("OpenClaw SSH command retains an unresolved virtual namespace")
	}
	return translated, nil
}

func validWorkspaceLeftBoundary(command string, position int) bool {
	return position == 0 || isWorkspaceDelimiter(command[position-1])
}

func validWorkspaceRightBoundary(command string, position int) bool {
	return position == len(command) || command[position] == '/' || isWorkspaceDelimiter(command[position])
}

func isWorkspaceDelimiter(value byte) bool {
	return strings.ContainsRune(" \t\n\r'\"`(){}[];|&<>=:,", rune(value))
}

func validVirtualizationWorkdir(value string) bool {
	if value == "/" {
		return true
	}
	if len(value) < 2 || value[0] != '/' || value[len(value)-1] == '/' {
		return false
	}
	for _, component := range strings.Split(value[1:], "/") {
		if component == "" {
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
