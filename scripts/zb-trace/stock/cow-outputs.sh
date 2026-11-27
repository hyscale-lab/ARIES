#!/usr/bin/env bash
# Hash outputs after collection so verification has no guest-memory footprint during replay.
set -euo pipefail
: "${1:?cohort directory}" "${2:?service directory}"
run=$1
service=$2
export XDG_CONFIG_HOME="$service/xdg"
cli="$(dirname "$service")/cli-bundle/aenv"
test -f "$run/measure/stop"
for task in wal-recovery-ordering session-window-debug calendar chapter-summary; do
    dir="$run/$task"
    test -s "$dir/completion.json"
    cwd=/app
    case "$task" in
        wal-recovery-ordering) files=(serializer.py) ;;
        session-window-debug) files=(app/events.py) ;;
        calendar) cwd=/; files=(/tmp_workspace/results/{decision_log.md,scheduled.ics,unscheduled.json}) ;;
        chapter-summary) cwd=/; files=(/tmp_workspace/linux_summary.md) ;;
    esac
    "$cli" exec "$(cat "$dir/restored-id")" runc exec --cwd "$cwd" zb-workload sha256sum "${files[@]}" >"$dir/output.sha256"
done
