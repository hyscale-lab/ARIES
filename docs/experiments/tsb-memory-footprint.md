# Step-scale tool-sandbox memory characterization

> Historical investigation from November 2026. The worker paths, admission
> decisions, authorizations and result claims below describe that investigation
> only. Its private run artifacts are not part of this source recovery. For the
> current source layout, dependency pins and VMM-owned tracing backend, use the
> [tracer source guide](memory-footprint-tracing.md). New runs require their own
> configuration and authorization.

This study starts at ARIES commit
`89b48a7f9174592bb71538bb5f0da3d46cc0dd26`. It asks whether accessed memory
is small relative to attributable host residency (H1), and whether page
membership changes between actual model/tool Steps (H2). Neither observation
establishes reclaimability or safe eviction. Calibration gates precede paid
benchmark admission; successful guest execution alone is insufficient.

Work is performed on the dedicated existing CloudLab worker `amd205`.
The control node is used only for file inspection and SSH. Worker source and
artifacts live at `/home/nehalem/aries`, branch `jy/tsb-mem-footprint`.
Worker installation details, exact hashes, configurations and commands are
retained in `runs/tsb-memory-footprint/preflight/stack-manifest.json` and
adjacent files. These private artifacts and credentials are ignored by Git.

## Profiles and admission

Research profiles are under `profiles/experiments/tsb-memory-footprint/`:

| Profile | Source task list | Nominal real attempts |
| --- | --- | ---: |
| `mock.json` | deterministic fix-git infrastructure smoke | 0 |
| `fix-git.json` | existing vHive fix-git profile | 1 |
| `five.json` | original five-task profile, original order | 5 |
| `twenty.json` | original twenty-task profile, original order | 20 |

All select persistent vHive and concurrency one. Each explicitly points to a
stage-specific override file enforcing **2 vCPUs and 4096 MiB** for every tool
sandbox. The five-task source concurrency was two. Stage-specific harness
resources and timeout overrides are retained; no task files or verifier
semantics are changed. Capacity fallbacks match the explicit overrides.
Effective Firecracker machine configuration must be checked per allocation.

The user subsequently selected `deepseek-flash` with `reasoning_effort: high`
for **all** real stages and authorized paid execution. This replaces the
original low setting for fix-git/five; twenty was already high. The nominal
schedule remains 26 attempts over 24 distinct tasks, with at most two
additional recovery attempts per real stage. Mock infrastructure runs are
separate (cap five). No currency cap was supplied. These bounds do not waive
measurement validity gates. Record per-stage effective deadlines and estimated
trace storage before admission, and account for failures rather than excluding
them. The private preflight manifest records current admission status; its
requested resources do not prove an actual allocation.

All 24 pinned task definitions request 10240 MiB disk and zero GPUs. Source
capacities differ; the common 2/4 allocation is an experimental change, not an
assertion that every task fits. Utilities, image digests and guest connectivity
still require runtime checks. Benchmark repository pin stays
`7131e4375048a0e408a8fb404b5f499d726b695b`.

## Calibration driver and access observer

Build and run on the worker only:

```sh
GOCACHE=$PWD/.cache/go-build GOMODCACHE=$PWD/.cache/go-mod   go build -o bin/tsb-vm ./scripts/tsb-vm
```

`tsb-vm -fixture STATIC_EXECUTABLE -output NEW_DIRECTORY` creates one
network-disabled, persistent 2-vCPU/4096-MiB calibration VM, uploads the
deterministic executable, and connects its stdin/stdout directly through the
native provider. It retains request, guest and native VM information, and
confirmed cleanup or a cleanup error. The fixture has CAP_SYS_ADMIN inside
this calibration guest so its own pagemap returns physical identities.
This capability is not added to production provider requests or benchmark
sandboxes. Calibration is a different workload from agent execution.

`scripts/tsb-access/quality-gate.json` declares target coverage, precision,
cadence and overhead requirements before target outcomes. Source metadata for
KVM slots is necessary: host PFNs and host virtual mappings are not stable
guest-page identities. Retain actual folio/huge-page granularity; do not expand
one observed region hit into invented individual 4-KiB hits. Record scan bounds
crossing Step boundaries as ambiguous. One shared observer timeline serves
concurrent Steps; never clear access bits independently for overlapping Steps.

The primary attempt is host page-idle tracking with explicit KVM slot mapping.
The supported fallback inspection checks guest page-idle availability. Aggregate
referenced/dirty/RSS counters are not a page-set fallback. Failed calibration
must remain in evidence and blocks a supposedly complete H1/H2 benchmark study.

## Detailed host accounting

`(*vhive.Manager).ObserveMemory` resolves the VMM through its native control
socket peer credentials and reads its parent shim. PID plus process start ticks
are checked before and after mapping scans. `smaps` records retain separate
RSS, PSS, private/shared, anonymous and huge-page fields in bytes, address
bounds, backing identity and flags. Missing fields, malformed sizes, permission
errors and vanished/reused processes fail that sample; they are not zeros.

`scripts/tsb-host` is a separate research collector for exact `aries.run`
labels in the `aries` namespace. It records continuous discovery scans, partial
per-runtime observations, errors and final termination, and continues observing
other runtimes after one observation fails:

```sh
GOCACHE=$PWD/.cache/go-build GOMODCACHE=$PWD/.cache/go-mod   go build -o bin/tsb-host ./scripts/tsb-host
sudo bin/tsb-host -run EXACT_RUN_ID -output NEW_HOST_JSONL   -interval 1s -duration 2h
```

This is **partial process-mapping accounting**, not total attributable host
memory. The VMM mappings already contain mapped guest RAM: do not add guest RAM
again to VMM RSS. RSS sums may double-count shared pages; PSS is a proportional
allocation, not an exact physical union. Host kernel allocations, cache outside
these mappings, shared containerd, bridge, harness and model-service overhead
remain separately unmeasured here. No whole-footprint denominator or H1 ratio
is manufactured. The existing generic vHive ResourceSource remains unsupported
until a compatible complete aggregate meaning is established.

## Step telemetry

The pinned OpenClaw image has native model/tool diagnostics, but existing
ARIES archives alone do not establish the requested Step contract. In
particular, model/tool spans lack a direct causal parent edge, and asynchronous
process-completion diagnostics omit the job and originating tool IDs.
Time-nearest SSH joins must remain ambiguous. See `scripts/tsb-steps/` for the
bounded metadata-only feasibility collector, reducer and native integration gap.
A partial diagnostic model interval is not a complete Step.

## Execution and final validation

Once gates pass, setup/run commands are the existing ARIES interface:

```sh
make build
sudo ./bin/aries setup profiles/experiments/tsb-memory-footprint/mock.json
# Start bin/aries-echo per docs/implementation/mock-llm.md, then:
sudo ECHO_API_KEY=mock-only ./bin/aries profiles/experiments/tsb-memory-footprint/mock.json
# Real stages, only after each predecessor/measurement gate:
sudo ./bin/aries profiles/experiments/tsb-memory-footprint/fix-git.json
sudo ./bin/aries profiles/experiments/tsb-memory-footprint/five.json
sudo ./bin/aries profiles/experiments/tsb-memory-footprint/twenty.json
```

These are execution instructions, not evidence that the stages ran.
The worker's private `DEEPSEEK_API.key` is root-owned mode 0600 for the
root runner. Never copy its content into manifests, commands in reports or Git.

Before implementation handoff, run `make build`, `make test`,
`make test-race`, `make lint`, `make integration` on a worker, plus the
documented opt-in real vHive lifecycle test. Inspect native VMs, shims,
containers, snapshots, networks, sockets and private credential staging after
runtime checks. Keep infrastructure success, task scores, measurement coverage
and cleanup outcomes separate. Report skipped, failed and unadmitted work.

## Results of the bounded investigation (6 November 2026)

The native sandbox path works for deterministic fix-git diagnostics, but the
access observer does not pass study admission. These are separate conclusions.

Two mock attempts completed harness execution, confirmed harness stop and bridge
revocation, ran the original verifier, and confirmed task/shared cleanup.
Both retained the same guest boot ID and a live background process across
commands. The first diagnostic's HTTPS command failed because the image has no
`curl`; the second used its existing Python standard library and received HTTP
200. Both graded zero, as expected: neither command attempted the Git solution.
The actual Firecracker API and guest observations confirmed two vCPUs and
4096 MiB (guest `MemTotal: 4038924 kB`). The normal benchmark sandbox did not
receive calibration-only CAP_SYS_ADMIN.

The partial host collector recorded 27 successful mapping samples and five
explicit startup/teardown errors in each mock. The first collector's scan-level
status described discovery only; the second includes failed runtime observations
in scan status. A review also added socket-peer/generation checks so capacity
and mapping data cannot be silently joined across different VMM generations.
These differences remain visible; the first trace is not retroactively relabeled.

Five separate calibration VMs were cleaned up. Attempts 001–002 could not obtain
guest physical identities under ordinary guest-container permissions. Attempt
003 passed 36 initial target cases. Attempt 004 exposed non-LRU unknowns.
Attempt 005 ran all ten schedules, 60 cases total, plus two full-resident scans.
No gate was relaxed to make those results pass.

One file-cache case had apparent recall 235/256 (91.796875%), below the
predeclared 95% threshold. Guest physical identities were recorded at preparation
and reuse, not rechecked after every access. Therefore the missing observations
cannot yet be attributed specifically to KVM TLB behavior; page-identity effects
remain unresolved. Unknown pages and changing coverage must not be interpreted
as membership turnover.

The whole-resident scan took about 2.25 CPU-seconds per sample. Actual cycles were
2.35 and 3.25 seconds for configured waits of 100 ms and one second, consuming
0.957 and 0.692 CPU cores against the predeclared 0.1-core cap. Peak observer RSS
reached 115.9 MiB. The desired scan cadence was not achieved, and simultaneous
whole-observer effects on tool latency were not established.

At the scan endpoints, 79,343 guest slots were resident; 62 shared/zero and seven
non-LRU pages were unknown. The resulting 99.913% endpoint coverage does not
bound transient residency. The logical resident slot count is not a unique
physical-memory denominator. The two KVM slots occupy one merged host VMA:
counting its smaps once per slot would double-count it. KSM and host swap were
off, and the inspected guest-RAM VMA had no THP or shared RSS. Shared/zero-page
observations still prevent an unqualified no-sharing claim. The guest page-idle
fallback was absent (`ENOENT`).

**H1 and H2 are untested for benchmark Sessions.** Real-agent stage accounting:
fix-git 0/1 attempted, five-task 0/5, twenty-task 0/20; zero paid retries.
All 26 planned Sessions remain unadmitted by the required measurement gate.
Neither mock success nor the calibration's small known target set establishes
Gate A. No Step distributions, workload page maps or turnover matrices are
fabricated from aggregate counters. The native Step collector/reducer has
fixture and plugin-registration evidence; its proposed source patch is unbuilt
and is not an implemented runtime integration.

## Rebuilding the evidence report

On the worker, retained raw artifacts are sufficient for analysis without an LLM:

```sh
python3 scripts/tsb-study/preflight.py   --output runs/tsb-memory-footprint/preflight/resolved-inputs-NEW.json
sudo python3 scripts/tsb-study/summarize.py --root runs/tsb-memory-footprint
python3 scripts/tsb-study/report.py --root runs/tsb-memory-footprint   --output runs/tsb-memory-footprint/report.html
```

The standalone HTML provides English/한국어 switching, a trial filter, calibration
recall plots, overhead, stage accounting and raw-source hashes. Canonical methods
remain English. This is a failed-admission report, not a completed twenty-task
study. `validation/status.txt` and individual logs record actual check attempts,
including prerequisite failures; do not infer a pass from this documentation.

## Smallest follow-up proposal

Use one fresh 2-vCPU/4096-MiB persistent calibration VM and the existing known
anonymous/file-cache schedules, with no LLM. Check guest PFNs before and after
each access schedule, retaining changed identities as unknown. Batch host
metadata reads in a native collector to test whether CPU cost can meet the
original limit without dropping page classes. Recheck apparent file-cache
misses and both configured cadences. Allow at most three paired repetitions,
30 minutes of worker time and 1 GiB of traces; do not change KSM, swap, THP,
reclaim or benchmark task semantics. Stop if correctness or overhead remains
outside the original gate. This is a proposed additional experiment, not an
executed result or a new eviction mechanism.

Separately, before claiming complete Steps, test/build the small proposed native
OpenClaw metadata patch and close its documented retry, auxiliary-call, clock,
shutdown and background-descendant gaps. Before H1, complete the declared host
component inventory and once-only attribution. The paid task rollout remains
contingent on these measurement gates.

## Final handoff decision

The user selected the failed-gate handoff; the proposed additional experiment
was not run. Final worker release checks (`make build`, `make test`,
`make test-race`, `make lint`, `make integration`) and the native lifecycle
test passed. The lifecycle test uses legacy 1-vCPU/1024-MiB defaults outside
the fixed-resource research stages. Browser assertions passed for the report
filter, chart and English/Korean toggle. Initial failed check logs are retained.
All logged evidence, reports, and a source recovery bundle are copied to the
control node under `runs/tsb-memory-footprint`; the API credential is excluded.
