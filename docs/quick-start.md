# Quick start: OpenClaw + Terminal-Bench 2 + DeepSeek

This guide runs the checked-in `fix-git` experiment from a clean ARIES clone.
Run every command from the repository root.

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

## 2. Build and prepare the profile

```sh
make build
./bin/aries setup profiles/openclaw-tb2-fix-git-deepseek.json
```

Setup strictly loads the profile and `configs/versions.json`, creates or
verifies the pinned Terminal-Bench checkout at `.cache/terminal-bench-2`, and
pulls the pinned task and OpenClaw images through the Docker Go SDK. It is safe
to run again. It refuses to replace a checkout at a different revision.

The equivalent Make target is:

```sh
make setup
```

## 3. Add the DeepSeek credential

The preferred source is the ignored repository-root file
`DEEPSEEK_API.key`:

```sh
install -m 600 /dev/null DEEPSEEK_API.key
${EDITOR:-vi} DEEPSEEK_API.key
chmod 600 DEEPSEEK_API.key
```

The file must be a current-user-owned, regular, non-symlink file with mode
`0600` and one nonempty line. ARIES never writes its value to JSON, logs,
Docker metadata, or results.

If the file is absent, ARIES reads `DEEPSEEK_API_KEY` from the environment. If
the file exists but is invalid, ARIES fails closed instead of falling back.

## 4. Run the experiment

```sh
./bin/aries profiles/openclaw-tb2-fix-git-deepseek.json
```

Use the binary produced by `make build` and keep `bin/aries-ssh` beside it.
The live DeepSeek run can incur API charges. ARIES first performs a bounded
model preflight, then runs OpenClaw, revokes its SSH access, and evaluates the
same still-running task sandbox.

## 5. Check the result

```sh
run_dir="$(ls -1dt runs/* | head -1)"
cat "$run_dir/live-validation.json"
cat "$run_dir/run-result.json"
cat "$run_dir/fix-git/evaluation/reward.txt"
cat "$run_dir/fix-git/bridge/tool-calls.jsonl"
cat "$run_dir/fix-git/harness/openclaw.json"
cat "$run_dir/aries.log"
```

A successful `fix-git` run has:

- successful model validation;
- separate successful harness, isolation, evaluation, observer, and cleanup
  outcomes in `run-result.json`;
- reward `1`; and
- completed tool calls in `tool-calls.jsonl`.

The run directory name contains `fix-git`, and task artifacts are grouped under
`fix-git/{harness,bridge,sandbox,monitor,evaluation}`. It contains the exact
placeholder-only rendered OpenClaw config, OpenClaw logs and telemetry when
available, replayable SSH tool inputs, Docker sandbox logs, one-second CPU and
memory samples, verifier stdout/stderr, and CTRF output. `aries.log` is the
structured Logrus run log.

## Troubleshooting

- **Docker permission or socket error:** run `docker info`; ARIES uses the local
  daemon at `/var/run/docker.sock`.
- **Repository-root or missing `aries-ssh` error:** rebuild with `make build`
  and run the real `./bin/aries` without renaming, moving, or symlinking it.
- **Credential error:** check ownership, mode `0600`, and one-line formatting
  of `DEEPSEEK_API.key`.
- **Model error:** inspect `live-validation.json` for authentication, rate
  limit, connectivity, or missing-model categories.
- **Terminal-Bench revision mismatch:** move the stale checkout aside, then
  rerun the profile setup command. Setup never deletes it automatically.
- **SSH timeout:** check host firewall rules and confirm containers can reach
  the Docker bridge gateway.
- **Suspected leak:** inspect `docker ps -a --filter label=aries.managed=true`
  and `docker network ls --filter label=aries.managed=true`.

For architecture and security boundaries, see [design.md](design.md).
