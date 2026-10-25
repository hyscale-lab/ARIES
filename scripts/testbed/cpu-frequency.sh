#!/usr/bin/env bash
# Holds this node's CPUs at one clock speed, so runs compared across tool
# sandboxes execute at the same frequency. Run it on every node that hosts a
# harness, sandbox or bridge, and on the runner.
#
#   scripts/testbed/cpu-frequency.sh status
#   sudo scripts/testbed/cpu-frequency.sh turbo-off   # or turbo-on
#   sudo scripts/testbed/cpu-frequency.sh pin [MHz]   # default: the highest
#                                                     # non-turbo frequency
#   sudo scripts/testbed/cpu-frequency.sh unpin
#
# Turn turbo off before pinning, so the default is the base clock. It uses the
# kernel's cpufreq interface, which covers intel_pstate, amd-pstate and
# acpi-cpufreq. A reboot undoes both settings.
set -euo pipefail

cpus=/sys/devices/system/cpu
saved=/run/aries-cpu-frequency.saved

# turbo_control prints the file that switches boost, its "off" and "on" values.
turbo_control() {
  if [ -f "$cpus/intel_pstate/no_turbo" ]; then
    echo "$cpus/intel_pstate/no_turbo 1 0"
  elif [ -f "$cpus/cpufreq/boost" ]; then
    echo "$cpus/cpufreq/boost 0 1"
  fi
}

policies() { ls -d "$cpus"/cpufreq/policy* 2>/dev/null; }

distinct() { for policy in $(policies); do cat "$policy/$1"; done | sort -u | tr '\n' ' '; }

status() {
  read -r file off _ <<<"$(turbo_control)" || true
  if [ -z "${file:-}" ]; then
    echo "turbo    : no control exposed; check the BIOS setting"
  elif [ "$(cat "$file")" = "$off" ]; then
    echo "turbo    : disabled"
  else
    echo "turbo    : enabled"
  fi
  echo "driver   : $(cat "$cpus/cpu0/cpufreq/scaling_driver" 2>/dev/null || echo none)"
  echo "governor : $(distinct scaling_governor)"
  echo "min kHz  : $(distinct scaling_min_freq)"
  echo "max kHz  : $(distinct scaling_max_freq)"
  # Lowest and highest current frequency across CPUs; equal once pinned.
  echo "now MHz  : $(awk '/^cpu MHz/ {printf "%d\n", $4}' /proc/cpuinfo | sort -n | sed -n '1p;$p' | tr '\n' ' ')"
}

require_root() { [ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }; }

set_turbo() {
  require_root
  read -r file off on <<<"$(turbo_control)" || true
  [ -n "${file:-}" ] || { echo "this node exposes no turbo control; check the BIOS setting" >&2; exit 1; }
  if [ "$1" = off ]; then echo "$off" >"$file"; else echo "$on" >"$file"; fi
  status
}

pin() {
  require_root
  [ -n "$(policies)" ] || { echo "this node exposes no cpufreq policies" >&2; exit 1; }
  local khz
  if [ $# -gt 0 ]; then
    case "$1" in ''|*[!0-9]*) echo "frequency must be a whole number of MHz" >&2; exit 2 ;; esac
    khz=$(($1 * 1000))
  else
    # With turbo off the driver caps the ceiling at the base clock.
    khz=$(cat "$cpus/cpu0/cpufreq/scaling_max_freq")
  fi
  # The settings from before the first pin, which unpin puts back.
  if [ ! -f "$saved" ]; then
    for policy in $(policies); do
      echo "$policy $(cat "$policy/scaling_governor") $(cat "$policy/scaling_min_freq") $(cat "$policy/scaling_max_freq")"
    done >"$saved"
  fi
  for policy in $(policies); do
    echo performance >"$policy/scaling_governor"
    # Lower the floor first so the ceiling never drops below it, then raise it.
    echo "$(cat "$policy/cpuinfo_min_freq")" >"$policy/scaling_min_freq"
    echo "$khz" >"$policy/scaling_max_freq"
    echo "$khz" >"$policy/scaling_min_freq"
  done
  status
}

unpin() {
  require_root
  [ -f "$saved" ] || { echo "not pinned by this script" >&2; exit 1; }
  while read -r policy governor min max; do
    echo "$(cat "$policy/cpuinfo_min_freq")" >"$policy/scaling_min_freq"
    echo "$max" >"$policy/scaling_max_freq"
    echo "$min" >"$policy/scaling_min_freq"
    echo "$governor" >"$policy/scaling_governor"
  done <"$saved"
  rm -f "$saved"
  status
}

case "${1:-}" in
  status) status ;;
  turbo-off) set_turbo off ;;
  turbo-on) set_turbo on ;;
  pin) shift; pin "$@" ;;
  unpin) unpin ;;
  *) sed -n '2,15s/^# \{0,1\}//p' "$0" >&2; exit 2 ;;
esac
