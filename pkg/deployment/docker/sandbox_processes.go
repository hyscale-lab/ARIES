package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

type processExecutor interface {
	Exec(context.Context, string, core.Command) (core.CommandResult, error)
}

// The scanner uses shell builtins to avoid creating processes while scanning.
// Exclude the current scanner and its ancestors, which are deployment exec
// wrappers; PID 1 is always preserved and additionally binds the baseline to the
// original process namespace. Process names may contain spaces and parentheses.
const sandboxProcessScript = `mode=$1
identity() {
 pid=$1
 if ! IFS= read -r stat <"/proc/$pid/stat"; then [ ! -d "/proc/$pid" ] && return 1; exit 125; fi
 rest=${stat##*) }
 set -- $rest
 [ "$#" -ge 20 ] || exit 125
 state=$1; parent=$2
 shift 19
 started=$1
 case "$started" in ''|*[!0-9]*) exit 125;; esac
 return 0
}
ancestors=" "
pid=$$
while [ "$pid" -gt 1 ]; do
 ancestors="$ancestors$pid "
 identity "$pid" || exit 125
 [ "$parent" != "$pid" ] || exit 125
 pid=$parent
done
if [ "$mode" = snapshot ]; then
 for entry in /proc/[0-9]*; do
  pid=${entry##*/}
  case "$ancestors" in *" $pid "*) continue;; esac
  identity "$pid" || continue
  case "$state" in Z|X) continue;; esac
  printf '%s %s\n' "$pid" "$started"
 done
 exit 0
fi
[ "$mode" = drain ] || exit 125
baseline="
"
while IFS=' ' read -r pid started extra; do
 [ -z "$extra" ] || exit 125
 case "$pid:$started" in *[!0-9:]*|:*|*:) exit 125;; esac
 baseline="$baseline$pid $started
"
done
identity 1 || exit 125
case "$baseline" in *"
1 $started
"*) ;; *) exit 125;; esac
stable=0
while [ "$stable" -lt 2 ]; do
 found=0
 for entry in /proc/[0-9]*; do
  pid=${entry##*/}
  [ "$pid" -gt 1 ] || continue
  case "$ancestors" in *" $pid "*) continue;; esac
  identity "$pid" || continue
  case "$state" in Z|X) continue;; esac
  case "$baseline" in *"
$pid $started
"*) continue;; esac
  found=1
  kill -KILL "$pid" 2>/dev/null || {
   identity "$pid" || continue
   case "$state" in Z|X) continue;; esac
   exit 125
  }
 done
 if [ "$found" -eq 0 ]; then stable=$((stable+1)); else stable=0; fi
 sleep 0.05
done
`

// snapshotSandboxProcesses must precede bridge exposure. A lost snapshot cannot
// be reconstructed after agent execution and is never replaced during retries.
func snapshotSandboxProcesses(ctx context.Context, executor processExecutor, id string) ([]deployment.ProcessIdentity, error) {
	result, err := executor.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", sandboxProcessScript, "aries-process-baseline", "snapshot"}, User: "0:0", Timeout: 30 * time.Second, OutputLimitBytes: 4 << 20})
	if err != nil {
		return nil, fmt.Errorf("snapshot sandbox processes: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, errors.New("snapshot sandbox processes failed")
	}
	var identities []deployment.ProcessIdentity
	seen := map[uint64]bool{}
	hasInit := false
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, errors.New("invalid sandbox process snapshot")
		}
		pid, e1 := strconv.ParseUint(fields[0], 10, 64)
		start, e2 := strconv.ParseUint(fields[1], 10, 64)
		if e1 != nil || e2 != nil || pid == 0 || start == 0 || seen[pid] {
			return nil, errors.New("invalid sandbox process identity")
		}
		seen[pid] = true
		hasInit = hasInit || pid == 1
		identities = append(identities, deployment.ProcessIdentity{PID: pid, StartTime: start})
		if len(identities) > 65536 {
			return nil, errors.New("sandbox process snapshot exceeds process limit")
		}
	}
	if !hasInit {
		return nil, errors.New("sandbox process snapshot does not identify PID 1")
	}
	return identities, nil
}

// revokeSandboxProcesses removes every live process created since admission,
// including setsid/double-fork descendants that escaped individual exec groups.
// Existing benchmark processes survive. The caller must first close admission
// and drain native sessions; ctx must bound repeated scans and backend cleanup.
func revokeSandboxProcesses(ctx context.Context, executor processExecutor, id string, baseline []deployment.ProcessIdentity) error {
	if len(baseline) == 0 || len(baseline) > 65536 {
		return errors.New("sandbox process baseline is missing or too large")
	}
	var input strings.Builder
	seen := map[uint64]bool{}
	hasInit := false
	for _, identity := range baseline {
		if identity.PID == 0 || identity.StartTime == 0 || seen[identity.PID] {
			return errors.New("invalid preserved sandbox process identity")
		}
		seen[identity.PID] = true
		hasInit = hasInit || identity.PID == 1
		fmt.Fprintf(&input, "%d %d\n", identity.PID, identity.StartTime)
	}
	if !hasInit {
		return errors.New("preserved sandbox process baseline has no PID 1")
	}
	result, err := executor.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", sandboxProcessScript, "aries-process-revoke", "drain"}, Stdin: []byte(input.String()), User: "0:0", Timeout: 30 * time.Second, OutputLimitBytes: 1 << 20})
	if err != nil {
		return fmt.Errorf("confirm assignment process termination: %w", err)
	}
	if result.ExitCode != 0 {
		return errors.New("assignment process termination remains unresolved")
	}
	return nil
}
