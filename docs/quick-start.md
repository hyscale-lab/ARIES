# Quick start: OpenClaw + Terminal-Bench 2 + DeepSeek

This guide runs a checked-in Terminal-Bench 2 experiment from a clean ARIES
clone. Run every command from the repository root.

## 1. Prerequisites

- Linux with a running local Docker Engine and access to `/var/run/docker.sock`.
- Go 1.26.5, Git, and Make. Go's toolchain selection can download 1.26.5 when
  an older Go launcher is installed.
- Network access to GitHub, GHCR, Docker Hub, and `https://api.deepseek.com`.
- A DeepSeek API key with access to the model named in the profile.

Verify Docker before continuing:

```sh
cd ~/projects/aries
docker info >/dev/null
```

ARIES currently validates the host bridge path on native Linux Docker. Docker
Desktop and rootless networking are not yet supported configurations.

## 2. Choose and prepare a profile

Use the one-task profile for the quickest first run:

```sh
make build
./bin/aries setup profiles/openclaw-tb2-fix-git-deepseek.json
```

To prepare the heterogeneous five-task subset instead:

```sh
make build
./bin/aries setup profiles/openclaw-tb2-five-deepseek.json
```

Setup strictly loads the selected profile and `configs/versions.json`, creates
or verifies the pinned Terminal-Bench checkout at `.cache/terminal-bench-2`,
and pulls only OpenClaw plus the selected task images through the Docker Go SDK.
It is safe to run again and refuses to replace a checkout at another revision.

The Make equivalent accepts either profile:

```sh
make setup
make setup PROFILE=profiles/openclaw-tb2-five-deepseek.json
```

To run another subset from the pinned revision, copy either profile and replace
`benchmark.tasks` with the desired task directory names. ARIES preserves the
listed order. The checked-in immutable image catalog covers every task in that
revision; no Go code change is required.

## 3. Add the DeepSeek credential

The preferred source for `./bin/aries` is the ignored repository-root file
`DEEPSEEK_API.key`:

```sh
install -m 600 /dev/null DEEPSEEK_API.key
${EDITOR:-vi} DEEPSEEK_API.key
chmod 600 DEEPSEEK_API.key
```

The file must be a current-user-owned, regular, non-symlink file with owner
read access, no group or world permissions, and one nonempty line. Modes `0400`
and `0600` are both valid. ARIES never writes the value to JSON, logs, Docker
metadata, or results.

If that repository-local file is unavailable, including when the binary is
installed elsewhere, ARIES reads `DEEPSEEK_API_KEY` from the environment. If a
repository-local file exists but is invalid, ARIES fails closed rather than
falling back.

## 4. Run the experiment

For the one-task example:

```sh
./bin/aries profiles/openclaw-tb2-fix-git-deepseek.json
```

For the five-task subset:

```sh
./bin/aries profiles/openclaw-tb2-five-deepseek.json
```

Keep `bin/aries-ssh` beside `bin/aries`. A live DeepSeek run can incur API
charges; the five-task profile also takes substantially longer and pulls more
images. ARIES first performs a bounded model preflight, then runs each task in
profile order. For each task it stops OpenClaw, revokes SSH access, and only
then evaluates the same still-running sandbox.

## 5. Check the result

For the one-task profile:

```sh
run_dir="$(ls -1dt runs/*-openclaw-tb2-fix-git-deepseek | head -1)"
cat "$run_dir/live-validation.json"
cat "$run_dir/run-result.json"
cat "$run_dir/fix-git/evaluation/reward.txt"
cat "$run_dir/fix-git/bridge/tool-calls.jsonl"
cat "$run_dir/fix-git/harness/openclaw.json"
cat "$run_dir/aries.log"
```

For a multi-task profile, `run-result.json` contains one result per task and
each task has its own readable directory:

```sh
run_dir="$(ls -1dt runs/*-openclaw-tb2-five-deepseek | head -1)"
find "$run_dir" -maxdepth 2 -type d | sort
find "$run_dir" -path '*/evaluation/reward.txt' -print -exec cat {} \;
```

A successful task has:

- successful model validation;
- separate successful harness, isolation, evaluation, observer, and cleanup
  outcomes in `run-result.json`;
- reward `1`; and
- completed tool calls in `bridge/tool-calls.jsonl`.

Each task directory contains the exact placeholder-only rendered
`harness/openclaw.json`, OpenClaw logs and telemetry when available, replayable
SSH tool inputs, Docker sandbox logs, one-second CPU and memory samples,
verifier stdout/stderr, and CTRF output. `aries.log` is the structured Logrus
run log.

## Troubleshooting

- **Docker permission or socket error:** run `docker info`; ARIES uses the local
  daemon at `/var/run/docker.sock`.
- **Missing `aries-ssh` error:** rebuild with `make build` and keep the helper
  beside the main binary.
- **Credential error:** check ownership, owner read access, absence of group or
  world permissions, and one-line formatting of `DEEPSEEK_API.key`.
- **Model error:** inspect `live-validation.json` for authentication, rate
  limit, connectivity, or missing-model categories.
- **Unknown task or missing image pin:** choose a task directory from the pinned
  checkout and keep `configs/versions.json` aligned when upgrading that pin.
- **Terminal-Bench revision mismatch:** move the stale checkout aside, then
  rerun the profile setup command. Setup never deletes it automatically.
- **SSH timeout:** check host firewall rules and confirm containers can reach
  the Docker bridge gateway.
- **Suspected leak:** inspect `docker ps -a --filter label=aries.managed=true`
  and `docker network ls --filter label=aries.managed=true`.

For architecture and security boundaries, see [design.md](design.md).
