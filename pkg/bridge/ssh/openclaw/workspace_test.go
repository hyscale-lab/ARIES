package openclaw

import (
	"reflect"
	"strings"
	"testing"
)

const (
	runtimeProbeScript   = `if [ -d "$1" ]; then printf "1\n"; else printf "0\n"; fi`
	runtimeProbeSentinel = "openclaw-sandbox-check"
)

func TestVirtualWorkspaceProtocolConstants(t *testing.T) {
	if virtualRuntimeRoot != "/aries/openclaw/openclaw-ssh-shared-8198076c" {
		t.Fatalf("virtualRuntimeRoot = %q", virtualRuntimeRoot)
	}
	if virtualWorkspace != virtualRuntimeRoot+"/workspace" {
		t.Fatalf("virtualWorkspace = %q", virtualWorkspace)
	}
}

func TestPinnedTransportControlLiterals(t *testing.T) {
	tests := map[string]struct {
		got, want string
	}{
		"probe script": {
			got: runtimeProbeScript, want: `if [ -d "$1" ]; then printf "1\n"; else printf "0\n"; fi`,
		},
		"probe sentinel":              {got: runtimeProbeSentinel, want: "openclaw-sandbox-check"},
		"workdir validation sentinel": {got: workdirValidationLabel, want: "openclaw-validate-workdir"},
		"directory clear sentinel":    {got: directoryClearLabel, want: "openclaw-sandbox-clear"},
		"directory upload sentinel":   {got: directoryUploadLabel, want: "openclaw-sandbox-upload"},
		"runtime remove script":       {got: runtimeRemoveScript, want: `rm -rf -- "$1"`},
		"runtime remove sentinel":     {got: runtimeRemoveLabel, want: "openclaw-sandbox-remove"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("pinned literal changed byte-for-byte:\n got %q\nwant %q", test.got, test.want)
			}
		})
	}
}

func TestPinnedOpenClaw202671ScriptsMatchOfficialSource(t *testing.T) {
	if workdirValidationScript != `set -e
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
printf "%s\n" "$current"` {
		t.Fatal("pinned workdir validation script changed byte-for-byte")
	}
	if ensureRemoteDirectoryScript != `set -e
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
done` {
		t.Fatal("pinned ensure-directory script changed byte-for-byte")
	}
}

func TestWorkdirValidationMapsOnlyPositionalPaths(t *testing.T) {
	for _, root := range []string{"/app/personal site", "/"} {
		for _, suffix := range []string{"", "/subdir", "/sub dir", "/missing", "/../escape"} {
			argv := []string{remoteShell, "-c", workdirValidationScript, workdirValidationLabel, virtualWorkspace + suffix, virtualWorkspace}
			plan := prepareRemoteCommand(remoteCommand{argv: argv}, root)
			wantTarget := strings.TrimSuffix(root, "/") + suffix
			if wantTarget == "" {
				wantTarget = "/"
			}
			want := []string{remoteShell, "-c", workdirValidationScript, workdirValidationLabel, wantTarget, root}
			if got := append([]string{plan.command.Path}, plan.command.Args...); !reflect.DeepEqual(got, want) || plan.suppressed {
				t.Fatalf("validation argv = %#v, want %#v", got, want)
			}
		}
	}
}

func TestTransportControlsAreSuppressedOnlyOnExactArgv(t *testing.T) {
	for _, argv := range [][]string{
		{remoteShell, "-c", directoryClearScript, directoryClearLabel, virtualSkillsWorkspace, virtualRuntimeRoot},
		{remoteShell, "-c", directoryUploadScript, directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot},
		{remoteShell, "-c", runtimeRemoveScript, runtimeRemoveLabel, virtualRuntimeRoot},
	} {
		plan := prepareRemoteCommand(remoteCommand{argv: argv}, "/workspace")
		if !plan.suppressed {
			t.Fatalf("control was not suppressed: %#v", plan)
		}
		for index := range argv {
			near := append([]string(nil), argv...)
			near[index] += "-near"
			// The decoder rejects invalid executable/shell flags; path/script
			// near matches must not be mistaken for native transport controls.
			if index < 2 {
				continue
			}
			plan := prepareRemoteCommand(remoteCommand{argv: near}, "/workspace")
			if plan.suppressed {
				t.Fatalf("near control suppressed: %#v", near)
			}
		}
	}
}

func TestOrdinaryCommandsPreserveBodiesAndEnvironment(t *testing.T) {
	for _, body := range []string{
		"printf '%s' '" + virtualWorkspace + "suffix'",
		"cat <<'EOF'\n" + virtualWorkspace + "/body\n" + virtualRuntimeRoot + "\nEOF",
		"cd " + virtualWorkspace + "/subdir && cat " + virtualWorkspace + "//input",
		"printf x > /app/output",
		"rm -rf -- " + virtualRuntimeRoot,
	} {
		for _, home := range []string{"/tmp", "/custom home", virtualWorkspace} {
			argv := []string{remoteEnv, "LANG=en_US.UTF-8", "EXTRA=value", "HOME=" + home, "OPENCLAW_SHELL=exec", remoteShell, "-c", body}
			remote, err := decodeRemoteCommand(encodeCanonicalTokens(argv))
			if err != nil {
				t.Fatal(err)
			}
			plan := prepareRemoteCommand(remote, "/app")
			if plan.command.Args[1] != body || plan.command.Env["HOME"] != home || plan.command.Env["EXTRA"] != "value" || plan.encoded != encodeCanonicalTokens(argv) {
				t.Fatalf("command changed: %#v", plan)
			}
		}
	}
	probe := []string{remoteShell, "-c", runtimeProbeScript, runtimeProbeSentinel, virtualRuntimeRoot}
	plan := prepareRemoteCommand(remoteCommand{argv: probe}, "/app")
	if got := append([]string{plan.command.Path}, plan.command.Args...); !reflect.DeepEqual(got, probe) {
		t.Fatalf("probe changed: %#v", got)
	}
}

func generatedArgv(remainder string) []string {
	return []string{
		remoteEnv,
		"PATH=/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin",
		"HOME=/tmp",
		"LANG=C.UTF-8",
		"OPENCLAW_SHELL=exec",
		remoteShell,
		"-c",
		"cd '" + virtualWorkspace + "' && " + remainder,
	}
}
