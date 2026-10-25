#!/usr/bin/env bash
# Turns turbo boost off, or back on, on this node, so runs compared across tool
# sandboxes execute at the same clock speed. Run it on every node that hosts a
# harness, sandbox or bridge, and on the runner.
#
#   scripts/testbed/turbo-boost.sh status
#   sudo scripts/testbed/turbo-boost.sh disable
#   sudo scripts/testbed/turbo-boost.sh enable
#
# It uses the kernel's cpufreq interface, which covers intel_pstate, amd-pstate
# and acpi-cpufreq. A reboot turns turbo back on.
set -euo pipefail

cpus=/sys/devices/system/cpu

# control prints the file that switches boost, its "off" value and its "on" value.
control() {
  if [ -f "$cpus/intel_pstate/no_turbo" ]; then
    echo "$cpus/intel_pstate/no_turbo 1 0"
  elif [ -f "$cpus/cpufreq/boost" ]; then
    echo "$cpus/cpufreq/boost 0 1"
  fi
}

status() {
  read -r file off _ <<<"$(control)" || true
  if [ -z "${file:-}" ]; then
    echo "turbo   : no control exposed; check the BIOS setting"
  elif [ "$(cat "$file")" = "$off" ]; then
    echo "turbo   : disabled"
  else
    echo "turbo   : enabled"
  fi
  echo "driver  : $(cat "$cpus/cpu0/cpufreq/scaling_driver" 2>/dev/null || echo none)"
  # Lowest and highest current frequency across CPUs.
  echo "now MHz : $(awk '/^cpu MHz/ {printf "%d\n", $4}' /proc/cpuinfo | sort -n | sed -n '1p;$p' | tr '\n' ' ')"
}

set_turbo() {
  [ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }
  read -r file off on <<<"$(control)" || true
  [ -n "${file:-}" ] || { echo "this node exposes no turbo control; check the BIOS setting" >&2; exit 1; }
  if [ "$1" = disable ]; then echo "$off" >"$file"; else echo "$on" >"$file"; fi
  status
}

case "${1:-}" in
  status) status ;;
  disable|enable) set_turbo "$1" ;;
  *) sed -n '2,11s/^# \{0,1\}//p' "$0" >&2; exit 2 ;;
esac
