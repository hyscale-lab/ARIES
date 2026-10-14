package openclaw

import (
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	virtualRuntimeRoot     = "/aries/openclaw/openclaw-ssh-shared-8198076c"
	virtualWorkspace       = virtualRuntimeRoot + "/workspace"
	virtualSkillsWorkspace = virtualWorkspace + "/.openclaw/sandbox-skills"

	workdirValidationScript = `set -e
target="$1"
root="$2"
case "$target" in /*) ;; *) echo "remote directory must be absolute: $target" >&2; exit 1 ;; esac
case "$root" in /*) ;; *) echo "remote root must be absolute: $root" >&2; exit 1 ;; esac
target="${target%/}"
root="${root%/}"
[ -n "$target" ] || target="/"
[ -n "$root" ] || root="/"
if [ "$root" != "/" ]; then
  case "$target/" in "$root"/*|"$root/") ;; *) echo "remote directory must stay under root: $target" >&2; exit 1 ;; esac
fi
for path_to_check in "$target" "$root"; do
  relative="${path_to_check#/}"
  while [ -n "$relative" ]; do
    part="${relative%%/*}"
    if [ "$part" = "$relative" ]; then relative=""; else relative="${relative#*/}"; fi
    [ -n "$part" ] || continue
    case "$part" in "."|"..") echo "unsafe remote directory component: $part" >&2; exit 1 ;; esac
  done
done
if [ -L "$root" ]; then echo "unsafe remote root symlink: $root" >&2; exit 1; fi
if [ ! -d "$root" ]; then echo "remote root not found: $root" >&2; exit 1; fi
canonical_root="$(cd "$root" && pwd -P)"
relative="${target#"$root"}"
relative="${relative#/}"
current="$canonical_root"
while [ -n "$relative" ]; do
  part="${relative%%/*}"
  if [ "$part" = "$relative" ]; then relative=""; else relative="${relative#*/}"; fi
  [ -n "$part" ] || continue
  if [ "$current" = "/" ]; then next="/$part"; else next="$current/$part"; fi
  if [ -L "$next" ]; then echo "unsafe remote directory symlink: $next" >&2; exit 1; fi
  if [ ! -d "$next" ]; then echo "remote directory not found: $next" >&2; exit 1; fi
  current="$next"
done
printf "%s\n" "$current"`
	workdirValidationLabel = "openclaw-validate-workdir"

	ensureRemoteDirectoryScript = `set -e
target="$1"
root="${2:-$1}"
case "$target" in /*) ;; *) echo "remote directory must be absolute: $target" >&2; exit 1 ;; esac
case "$root" in /*) ;; *) echo "remote root must be absolute: $root" >&2; exit 1 ;; esac
target="${target%/}"
root="${root%/}"
[ -n "$target" ] || target="/"
[ -n "$root" ] || root="/"
case "$target/" in "$root"/*|"$root/") ;; *) echo "remote directory must stay under root: $target" >&2; exit 1 ;; esac
for path_to_check in "$target" "$root"; do
  relative="${path_to_check#/}"
  while [ -n "$relative" ]; do
    part="${relative%%/*}"
    if [ "$part" = "$relative" ]; then relative=""; else relative="${relative#*/}"; fi
    [ -n "$part" ] || continue
    case "$part" in "."|"..") echo "unsafe remote directory component: $part" >&2; exit 1 ;; esac
  done
done
if [ -L "$root" ]; then echo "unsafe remote root symlink: $root" >&2; exit 1; fi
mkdir -p -- "$root"
canonical_root="$(cd "$root" && pwd -P)"
relative="${target#"$root"}"
relative="${relative#/}"
current="$canonical_root"
while [ -n "$relative" ]; do
  part="${relative%%/*}"
  if [ "$part" = "$relative" ]; then relative=""; else relative="${relative#*/}"; fi
  [ -n "$part" ] || continue
  if [ "$current" = "/" ]; then next="/$part"; else next="$current/$part"; fi
  if [ -L "$next" ]; then echo "unsafe remote directory symlink: $next" >&2; exit 1; fi
  if [ -e "$next" ]; then
    if [ ! -d "$next" ]; then echo "unsafe remote directory component: $next" >&2; exit 1; fi
  else
    mkdir -- "$next"
  fi
  current="$next"
done`
	directoryClearScript = ensureRemoteDirectoryScript + `
find "$1" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +`
	directoryClearLabel   = "openclaw-sandbox-clear"
	directoryUploadScript = ensureRemoteDirectoryScript + `
tar -xf - -C "$1"`
	directoryUploadLabel = "openclaw-sandbox-upload"
	runtimeRemoveScript  = `rm -rf -- "$1"`
	runtimeRemoveLabel   = "openclaw-sandbox-remove"
)

type preparedRemoteCommand struct {
	command       core.Command
	encoded       string
	workspaceHome string
	suppressed    bool
}

func prepareRemoteCommand(remote remoteCommand, workdir string) preparedRemoteCommand {
	if len(remote.argv) == 6 && matchesExactArgv(remote.argv[:4], remoteShell, "-c", workdirValidationScript, workdirValidationLabel) && remote.argv[5] == virtualWorkspace {
		// These are provider-owned positional paths, not text in the agent's
		// shell body. Validate the real task root and retain native rejection
		// of missing directories, symlinks, and unsafe descendant components.
		target := remote.argv[4]
		if target == virtualWorkspace || strings.HasPrefix(target, virtualWorkspace+"/") {
			argv := append([]string(nil), remote.argv...)
			argv[4] = strings.TrimSuffix(workdir, "/") + strings.TrimPrefix(target, virtualWorkspace)
			if argv[4] == "" {
				argv[4] = "/"
			}
			argv[5] = workdir
			return preparedFromArgv(argv, workdir, false)
		}
	}
	if matchesExactArgv(remote.argv, remoteShell, "-c", directoryClearScript, directoryClearLabel, virtualSkillsWorkspace, virtualRuntimeRoot) ||
		matchesExactArgv(remote.argv, remoteShell, "-c", directoryUploadScript, directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot) ||
		matchesExactArgv(remote.argv, remoteShell, "-c", runtimeRemoveScript, runtimeRemoveLabel, virtualRuntimeRoot) {
		return preparedFromArgv(remote.argv, workdir, true)
	}
	return preparedFromArgv(remote.argv, workdir, false)
}

func preparedFromArgv(argv []string, workdir string, suppressed bool) preparedRemoteCommand {
	remote := remoteCommand{argv: append([]string(nil), argv...)}
	command := remote.command(workdir)
	return preparedRemoteCommand{
		command:       command,
		encoded:       encodeCanonicalTokens(argv),
		workspaceHome: command.Env["HOME"],
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
