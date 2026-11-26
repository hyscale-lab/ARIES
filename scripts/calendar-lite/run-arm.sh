#!/bin/bash
set -eu
[[ $EUID == 0 ]] || { echo "run inside the private worker directory as root" >&2; exit 2; }
exec 9>.aries-lite.lock
flock -n 9 || exit 1
label=${1:?arm name}
profile=${2:?on or off}
arm=${3:?control or checkpoint}
[[ $label =~ ^[a-zA-Z0-9][a-zA-Z0-9_-]*$ && $profile =~ ^(on|off)$ && $arm =~ ^(checkpoint|control)$ ]]
if [[ -e $label || -e $label.qcow2 || -e qemu.pid ]]; then
  echo "run label, overlay or VMM is already in use" >&2
  exit 1
fi
qemu-img create -q -f qcow2 -F qcow2 -b prepared.qcow2 "$label.qcow2"
cleanup() {
  if [[ -s qemu.pid ]] && grep -zFxq "file=$label.qcow2,if=virtio,format=qcow2,cache=none" "/proc/$(cat qemu.pid)/cmdline"; then
    systemctl stop zb-calendar-lite-vmm
  fi
}
trap cleanup EXIT
trap 'exit 143' TERM
trap 'exit 130' INT
./boot.sh "$label.qcow2" "$profile"
for _ in {1..60}; do
  if ./guest true 2>/dev/null; then break; fi
  sleep 1
done
./guest 'cloud-init status --wait && sync'
python3 arm.py "$label.qcow2" "$profile" "$arm" "$label"
./guest 'shutdown -h now' || true
for _ in {1..120}; do
  test -e qemu.pid || exit 0
  sleep .25
done
exit 1
