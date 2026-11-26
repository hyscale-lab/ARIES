#!/bin/bash
# Run from the node-local pilot directory. Give each runnable arm its own disk.
set -eu
disk=${1:?qcow2 disk}
report=${2:?on or off}
load=${3:-}
args=()
if [[ -n $load ]]; then args=(-loadvm "$load"); fi
if [[ -e bzImage ]]; then
  cmdline="root=LABEL=cloudimg-rootfs ro console=ttyS0 panic=1"
  if [[ $report == on ]]; then
    cmdline+=" page_reporting.page_reporting_order=6 damon_reclaim.enabled=Y damon_reclaim.skip_anon=Y damon_reclaim.min_age=100000"
    cmdline+=" damon_reclaim.quota_ms=20 damon_reclaim.quota_sz=1073741824 damon_reclaim.quota_reset_interval_ms=500"
    cmdline+=" damon_reclaim.wmarks_high=990 damon_reclaim.wmarks_mid=990 damon_reclaim.wmarks_low=200 damon_reclaim.wmarks_interval=1000000"
  fi
  args+=(-kernel bzImage -initrd initrd -append "$cmdline")
fi
sudo -n systemd-run --quiet --collect --unit=zb-calendar-lite-vmm --service-type=forking --working-directory="$PWD" qemu-system-x86_64 -enable-kvm -cpu host -machine pc -smp 1 -m 512 \
  -drive file="$disk",if=virtio,format=qcow2,cache=none \
  -drive file=seed.img,if=virtio,format=raw,readonly=on \
  -device virtio-balloon-pci,free-page-reporting="$report" \
  -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:22405-:22 \
  -qmp unix:qmp.sock,server=on,wait=off -pidfile qemu.pid -display none \
  -serial file:serial.log -daemonize "${args[@]}"
