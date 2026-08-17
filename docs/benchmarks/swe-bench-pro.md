# SWE-bench Pro

ARIES supports the 731-task public open-source split of SWE-bench Pro. The
upstream benchmark describes 1,865 tasks in total: 731 public open-source
tasks, 858 held-out open-source tasks, and 276 commercial tasks. ARIES cannot
load the held-out or commercial tasks because those task records and verifier
inputs are not part of the public dataset.

## Pinned inputs and task construction

`configs/versions.json` pins two independent upstream repositories:

- dataset: `ScaleAI/SWE-bench_Pro` at
  `7ab5114912baf22bb098818e604c02fe7ad2c11f`;
- evaluator: `scaleapi/SWE-bench_Pro-os` at
  `ca10a60a5fcae51e6948ffe1485d4153d421e6c5`.

Setup installs both checkouts under `.cache/swe-bench-pro`, resolves the
dataset's Git LFS Parquet object, and accepts an existing cache only when both
repositories are clean and at the exact pins. Task loading requires the pinned
Parquet schema and exactly 731 unique rows. It also verifies every selected
task's task-specific `run_script.sh` and `parser.py` in the evaluator checkout.

For each selected row, ARIES gives the harness only this prompt:

1. `problem_statement`;
2. a `Requirements:` section containing `requirements`;
3. a `New interfaces introduced:` section containing `interface`.

The gold patch, test patch, gold revision, test lists, and evaluator files stay
benchmark-private. The task image is
`docker.io/jefzda/sweap-images:<dockerhub_tag>` from that row, and the repository
workdir is `/app`. The benchmark defaults are four CPUs, 30 GiB memory, 20 GiB
storage, a one-hour agent timeout, and network access. Runtime overrides can
reduce the sandbox resources or change the agent timeout without changing task
meaning.

## Setup and run

Prerequisites beyond the normal ARIES requirements are:

- Git LFS for the pinned public Parquet file;
- registry access to `docker.io/jefzda/sweap-images`;
- a Linux host able to run the published `linux/amd64` task images;
- enough disk and memory for the selected repository image.

Build and prewarm either checked-in one-task profile from the repository root:

```sh
make build
./bin/aries setup profiles/openclaw-sbpro-smoke1-deepseek.json
# Or use Hermes:
./bin/aries setup profiles/hermes-sbpro-smoke1-deepseek.json
```

`setup` is idempotent. It installs or verifies the two pinned repositories and
pulls the selected task and harness images, but does not contact the external
model endpoint or admit task work. Run the same profile with:

```sh
./bin/aries profiles/openclaw-sbpro-smoke1-deepseek.json
# Or use Hermes:
./bin/aries profiles/hermes-sbpro-smoke1-deepseek.json
```

The example uses DeepSeek and therefore needs the credential described in the
[quick start](../quick-start.md#external-deepseek). It can incur API charges.
To select another public task, copy the profile and replace
`benchmark.tasks` with one or more `instance_id` values from the pinned public
dataset. Profile order, repetition, concurrency, and runtime overrides follow
the normal ARIES command semantics.

## Isolation and evaluation

The public task images contain repository history used to construct the
benchmark. Before the harness receives bridge access, ARIES:

1. resets the repository to the row's `base_commit` and proves it is clean;
2. checks out exactly the official gold-commit verifier files and snapshots
   them privately;
3. privately snapshots ignored build artifacts already present in the image;
4. restores the base worktree and removes verifier staging data;
5. removes Git remotes, refs, reflogs, and unreachable future objects, then
   proves the gold revision is not locally reachable;
6. privately snapshots the sanitized Git metadata, transfers `/app` to the
   numeric agent identity `65532:65532`, and proves the agent can write the
   worktree but cannot write the trusted Git, shell, tar, or Python runtimes.

The verifier, ignored-build, and sanitized-Git snapshots are host artifacts
outside both the task and harness containers. They are mode `0600` under a
mode `0700` private directory. The harness container has no bind mount to the
run output directory. Docker applies `no-new-privileges` to task containers
using the non-root agent identity and positively confirms the option through
post-start container inspection before returning the live sandbox.
Benchmark-owned preparation and evaluation commands explicitly use root.

This is local hardening, not an embargo on public information. SWE-bench Pro is
a public benchmark and the task network remains enabled so the harness can use
the configured model endpoint and ordinary network tools. A deliberately
adversarial agent can add a remote or retrieve public upstream repositories,
commits, datasets, or discussions. Do not use the public split as a confidential
test set.

Only after the Runner positively stops the harness and revokes the bridge does
ARIES evaluate the still-running task sandbox. It captures staged, tracked, and
untracked candidate changes against the privately restored sanitized Git
baseline. The raw download is bounded to 16 MiB before host writes complete,
and binary patch sections are removed to match the evaluator policy. Evaluation
then restores the pinned base plus the image's initial ignored build artifacts,
applies the candidate, and injects the private verifier files, task-specific
script, and parser. Harness-created ignored artifacts are removed before the
initial image snapshot is restored, which makes evaluation start from the
fresh-image build baseline rather than agent-created caches.

Before any private verifier input is staged, and again after the test process
returns, ARIES kills and positively confirms the absence of every process owned
by the agent UID. Verifier paths are installed only through non-symlink parent
directories and become root-owned read-only files. The test script runs as the
non-root agent; its stdout and stderr stream directly to mode-`0600` host
artifacts with a 256 MiB per-stream bound. The parser then runs as root with an
empty environment, isolated Python mode, and a root-only script. Private
container staging is removed and positively proved absent on every evaluation
return path.

The pinned script runs only the row's `selected_test_files_to_run`. The pinned
parser converts its output to structured test records. ARIES reports score and
reward `1` with succeeded evaluation/verifier statuses only when every test in
both `FAIL_TO_PASS` and `PASS_TO_PASS` is reported `PASSED`; otherwise it
reports score and reward `0` with failed statuses. Infrastructure, pin,
isolation, or malformed-parser failures remain evaluator errors rather than
being silently treated as an unresolved task.

Private evaluation artifacts are retained under each occurrence's
`evaluation/` directory: raw and effective candidate patches, verifier stdout
and stderr, parser output, and a resolution reason. Review these private run
artifacts before sharing them.

## Scope, architecture, and licensing

The adapter uses the existing `Benchmark` role; it adds no fifth Runner role.
Docker lifecycle and image pulls remain owned by the Docker sandbox through the
Moby Go SDK, and evaluator execution remains independent of harness success.

ARIES's MIT code license does not relicense the benchmark dataset, evaluator,
task images, or repositories represented by the tasks. The upstream evaluator
repository is MIT-licensed. The public dataset repository does not declare one
license covering all included material; task repositories and images may have
their own terms. Review and comply with the upstream dataset card, evaluator
license, image terms, and each task repository's license before redistribution
or commercial use.
