# Guest memory telemetry

Build the static Linux amd64 probe from the ARIES root.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
  -o /tmp/zb-guest ./scripts/zb-guest
sha256sum /tmp/zb-guest
```

Stage the binary at `/usr/local/bin/zb-guest` in the diagnostic image. Record
the diagnostic image and executable hashes. Keep the process inside the guest
and take observations while its vCPUs are running. Use the replay driver to
bracket each request with its own host monotonic timestamps. The probe also
records guest monotonic times for the full capture and the PFN scan. The two
clock origins are independent. Record probe work separately from tool times
and tracing freeze times.

## Snapshot

```sh
/usr/local/bin/zb-guest snapshot
/usr/local/bin/zb-guest snapshot --pfn-start 0 --pfn-count 524288
```

The default snapshot reads guest meminfo, buddyinfo, vmstat, swaps, boot
settings, kernel release, the probe's mount namespace, and readable DAMON
sysfs module parameters and administrative state. Values retain their
original units. A source that is missing, inaccessible, or above its size
limit is recorded with its error. DAMON sampling reads existing counters
without refreshing them, which can leave exposed counters stale. It skips
per-region tried-region details. Availability of a sysfs directory or
parameter does not establish feature activation or savings.

A PFN request walks guest `/proc/kpageflags` in 32 KiB batches without reading
the corresponding guest memory bytes. The input range is capped at 2,097,152
PFNs and the result at 262,144 runs. Each run contains equal raw flags and
covers a half-open guest PFN interval. The response gives requested and
observed endpoints and a completion state. End of file, insufficient
privilege, and limits leave unknown coverage. A 2 GiB guest with 4 KiB pages
and zero-based contiguous guest RAM can use the sample range above. Use the
registered RAM spans for layouts with holes.

Convenience labels report flags that the guest kernel exposes. A buddy or
slab bit classifies only the PFN that carries the bit. Do not extend a buddy
head to its unspecified neighbors or treat a missing bit as allocated.
`buddyinfo` supplies free-block counts by zone and order. Raw masks retain
compound and other overlapping flags. A file-cache candidate is a guest LRU
page with up-to-date data and without anonymous, swap-backed, or swapcache
flags. It does not establish regular-file ownership, durability, host
residency, snapshot equivalence, or recoverability. PFN ranges also cannot
establish guest object identity across samples.

## Local socket

The same snapshot is available over a Unix socket in a private guest
directory. Start it under a supervisor that can stop the daemon before the
diagnostic sandbox exits.

```sh
probe_dir=$(mktemp -d /tmp/zb-guest.XXXXXX)
/usr/local/bin/zb-guest serve --socket "$probe_dir/telemetry.sock"
# Send from a second guest exec while the daemon is serving.
/usr/local/bin/zb-guest request --socket "$probe_dir/telemetry.sock"
```

The socket accepts one fixed snapshot request and uses the server's options.
Guest exec can invoke one-shot snapshots if a daemon is unnecessary. Enable
PFN scans and daemon deployment on diagnostic runs first and measure their
perturbation before using them for comparative latency.

## File calibration

```sh
/usr/local/bin/zb-guest calibrate --directory /path/on/filesystem-under-test
```

The probe creates one temporary file and removes it at exit. It reports
mapping state before touch, after a known read of synced bytes, after a
buffered shared-mapping write, and after explicit sync. Each stage records
content hashes where touched, the mapping's guest PFN and flags when visible,
and the filesystem, mount, and available DAX evidence. The private-file cache
drop request is advisory. This calibration checks the interpretation of guest
file flags. Estimating a VM-dirty and filesystem-clean intersection still
requires contemporaneous host/KVM or snapshot-equivalence evidence for the
same GPA and page lifetime. The stock hook alone cannot establish it.

See [Linux pagemap and page flags](https://docs.kernel.org/admin-guide/mm/pagemap.html)
and [DAMON administration](https://docs.kernel.org/admin-guide/mm/damon/usage.html).
