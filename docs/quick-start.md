# Quick start: OpenClaw + Terminal-Bench 2

Run one checked-in task with either DeepSeek or a local SGLang model. Choose
one option below, then inspect its result. Run commands from the repository root.

## Prerequisites

- Linux with a running **local Docker Engine** and access to `/var/run/docker.sock`.
  Harness and sandbox must use the same local daemon. Remote Docker servers,
  Docker Desktop, rootless networking, and mixed deployment backends are unsupported.
- Go 1.26.5, Git, and Make. Go's toolchain selection can download 1.26.5 when
  an older Go launcher is installed.
- Network access to GitHub, GHCR, Docker Hub, and the configured model endpoint.

From your ARIES checkout:

```sh
docker info >/dev/null
make build
```

Keep `bin/aries-ssh` beside `bin/aries`; the build produces both.

## Option A: DeepSeek

You need a DeepSeek API key with access to the profile's model. This option can
incur model API charges.

### Configure the credential

The example profile already selects the model and endpoint. Store your key in
the ignored repository-root `DEEPSEEK_API.key` file. In Bash:

```sh
echo 'api_key' > DEEPSEEK_API.key
chmod 600 DEEPSEEK_API.key
```

The file must be owned by you, regular (not a symlink), owner-readable, and have
no group/world permissions. Keep the key out of profile JSON and shared output.
For environment credentials and other model backends, see
[model configuration](configuration.md#model-backends).

### Run one task

```sh
./bin/aries profiles/openclaw-tb2-fix-git-deepseek.json
```

ARIES prepares the pinned benchmark checkout and required images, validates the
model, and runs the task. The first run can take longer because of downloads.
After agent execution it confirms harness stop and bridge revocation, evaluates
the same sandbox, and cleans up. Preparation refuses to replace a checkout at a
different revision.

Optional: prewarm benchmark data and images without contacting the model service:

```sh
./bin/aries setup profiles/openclaw-tb2-fix-git-deepseek.json
```

## Option B: Local SGLang

ARIES can start and stop a local SGLang process for the experiment. You need a
Python environment with SGLang installed, NVIDIA drivers and `nvidia-smi`, and a
GPU with enough memory for `Qwen/Qwen3-8B`. Make the model weights available to
that environment beforehand; ARIES does not install SGLang or prepare weights.

### Create a managed profile

The checked-in SGLang profile selects external mode. Copy it before making
changes:

```sh
mkdir -p .cache
cp profiles/openclaw-tb2-fix-git-sglang.json .cache/openclaw-tb2-fix-git-sglang.json
```

Open `.cache/openclaw-tb2-fix-git-sglang.json` in your editor. Replace its
`runtime` and `model` sections with the following, keeping the rest of the
profile unchanged. This is a profile fragment, not a complete file:

```json
{
  "runtime": {
    "backend": "sglang",
    "mode": "managed",
    "config": {
      "file": "../configs/sglang/qwen3-8b-local.yaml",
      "executable": "/absolute/path/to/venv/bin/python",
      "startup_timeout": "15m",
      "stop_timeout": "1m",
      "gpu_indices": [0]
    }
  },
  "model": {
    "base_url": "http://sglang.local:30000/v1",
    "api_key_env": "SGLANG_API_KEY",
    "id": "Qwen/Qwen3-8B"
  }
}
```

Set `runtime.config.executable` to the Python executable in your SGLang
environment. Replace `sglang.local` in `model.base_url` with this machine's
hostname or IP reachable from both the ARIES host and Docker harness containers.
Do not use `localhost` or `127.0.0.1`: inside a harness container, those addresses
refer to the container itself.

This uses the checked-in YAML for `Qwen/Qwen3-8B` on GPU 0, listening on port
30000. Keep that port free and reachable from the containers. The HTTP example
is for a trusted local network with a non-secret placeholder credential; use
HTTPS for remote or credentialed endpoints. The YAML binds to `0.0.0.0`, so
restrict access to the model server to your trusted network.

### Run one task

```sh
export SGLANG_API_KEY=unused-local-token
./bin/aries .cache/openclaw-tb2-fix-git-sglang.json
```

Do not launch SGLang separately for this option. ARIES starts the server, waits
for health and model validation, runs the task, and stops the server afterward.
The first launch can take time to load the model. Server logs are saved under
`sglang/stdout.log` and `sglang/stderr.log` in the run directory.

See [managed SGLang configuration](configuration.md#managed-sglang) for GPU
selection, model changes, and timeout rules. If you already operate a server,
use [external SGLang](configuration.md#external-sglang) instead.

## Inspect the result

Results are saved under `runs/`, in a directory named with the run timestamp and
profile name. List the directories, newest first:

```sh
ls -1dt runs/*/
```

Choose the directory for your DeepSeek or SGLang run, then replace the placeholder
below with its name:

```sh
run_dir="runs/REPLACE_WITH_RUN_DIRECTORY"
cat "$run_dir/live-validation.json"
cat "$run_dir/run-result.json"
```

Check each outcome separately: harness success does not imply task correctness.
A passing task has confirmed isolation, successful evaluation with reward `1`,
and successful cleanup. Use [run results and troubleshooting](run-results.md)
for task logs, telemetry, verifier output, and failure diagnosis. Artifacts may
contain private task or model content; review them before sharing.

## Next experiments

- **Hermes:** run `profiles/hermes-tb2-fix-git-deepseek.json` with the same key.
- **More tasks:** run `profiles/openclaw-tb2-five-deepseek.json`; it pulls more
  images, uses concurrent execution, and can incur more API charges.
- **Local or other model services:** configure [external SGLang](configuration.md#external-sglang)
  or an [OpenAI-compatible endpoint](configuration.md#external-openai-compatible-server).
- **Custom experiments:** use the [configuration reference](configuration.md)
  for deployment, resource limits, task arrivals, and harness settings.
- **Other benchmarks and modes:** see [supported implementations](supported.md).
