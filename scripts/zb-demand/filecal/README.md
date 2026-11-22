# Ordinary-file backing and snapshot byte comparison

This standalone fixture uses the same vHive VM lifecycle and verified
KVM RAM-slot addresses as the replay. Its guest task sleeps. A small
service inside a disposable guest root accepts fixed diagnostic actions
over the host-only Firecracker vsock. The measured replay runs never use
that service or this diagnostic root.

Build both programs and prepare an isolated copy of the worker stack.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
  -o "$STUDY_STACK/tools/zb-filecal-guest" ./scripts/zb-demand/filecal/guest
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
  -o "$STUDY_STACK/tools/zb-filecal-driver" ./scripts/zb-demand/filecal/driver
scripts/zb-demand/filecal/install-root.sh "$ORIGINAL_ROOT" \
  "$STUDY_STACK/tools/zb-filecal-guest" "$STUDY_STACK/runtime/common-rootfs.ext4"
scripts/zb-trace/provision_baseline.sh "$STUDY_STACK" "$UNIT" "$PROFILE" v1
```

Stage this directory under `tools/scripts/zb-demand/filecal/` and the
trace scripts under `tools/scripts/zb-trace/`. Then run
`scripts/zb-demand/filecal/run.sh "$STUDY_STACK" "$UNIT" file-calibration v1`.
The fifth and sixth arguments can supply a private bpftrace installation
and library directory. The same commands accept `v4` for the optimized
native-checkpoint path. Preserve the original root and give the calibrated
root its own unit and storage. Record their hashes alongside the probe
and result hashes.

The probe opens a new ordinary file on the guest's writable root overlay
and directly writes one page at offset 4096. Direct synced readback proves
the initial backing bytes. The probe retains a normal shared mapping,
and mincore and guest pagemap confirm an absent file-cache page and PTE.
The driver snapshots and restores, and checks both observations again.

The fixture then reads the file, changes one byte through the mapping,
and runs synchronous writeback. At each stage it reports the retained
file identity, file offset, actual guest PFN and flags, and the digest of
the current mapped bytes. The final stage reads the synchronized file
page back through an O_DIRECT descriptor. Raw page bytes stay within
the diagnostic hosts.

Between actions, the host maps the reported guest frame through the
restored KVM slot while the VM is paused. It checks the host PTE before
reading, rechecks frame identity, and compares the live page against the
same offset in the private memory snapshot. This check requires a captured
single contiguous zero-based RAM slot and an exact-length memory file.
The fixture records whether the host PFN is the same as at the first
read. It rejects missing backing, unsupported mapping, incorrect bytes,
or mismatched file or guest PFN identity.

A clean read or synced write can match current filesystem backing while
differing from the VM snapshot. The guest diagnostic demonstrates that
intersection for the controlled page. It does not quantify the
intersection across the measured workload or imply that zeroing its live
VMM frame would preserve guest state.
