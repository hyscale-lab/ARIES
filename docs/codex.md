# Codex with a Responses API model server

ARIES supports the unmodified Codex CLI `0.157.1` as a text harness paired
with `codex-ssh`. This version's experimental native remote executor carries
shell and filesystem tools over SSH into the task container. The harness and
evaluator therefore operate on the same task files, with the usual harness
stop and bridge revocation gates before evaluation.

This adds a harness and bridge, not a benchmark adapter. RoadmapBench is not
implemented. A particular Qwen checkpoint is usable only when its serving
stack implements the Responses API and the native tool-call formats expected
by this Codex version; no live Qwen result is implied by the deterministic
integration test.

## Prepare the pinned CLI

Linux, Docker, and the normal ARIES build prerequisites are required. Use the
official static musl binary matching both the host and task-image architecture.
For `linux/amd64`, run from the ARIES repository:

```sh
mkdir -p .cache/codex/0.157.1
curl -fL --retry 3 \
  https://github.com/openai/codex/releases/download/rust-v0.157.1/codex-x86_64-unknown-linux-musl.tar.gz \
  -o .cache/codex/0.157.1/release.tar.gz
printf '%s  %s\n' \
  e98c1e8e028e8137fa2d2415c82ec58e7b3701a627e3554aace5b3ca31454af2 \
  .cache/codex/0.157.1/release.tar.gz | sha256sum -c -
tar -xzf .cache/codex/0.157.1/release.tar.gz -C .cache/codex/0.157.1
mv .cache/codex/0.157.1/codex-x86_64-unknown-linux-musl .cache/codex/0.157.1/codex
make build
```

The checksum above identifies the archive used for validation. ARIES rejects
scripts, dynamic executables, symlinks, and a staged CLI that reports a
different version. `make build` also builds the static `aries-codex-ssh`
client and `aries-codex-exec` supervisor; keep both beside `bin/aries`.
The version and base image are declared in `configs/versions.json`.
The default Debian slim image supports local HTTP endpoints. For HTTPS, select
a pinned harness image containing the required CA certificates; ARIES does not
install packages into that image or disable TLS verification.

## Configure and run

Edit `profiles/codex-tb2-fix-git-openai.json`:

- Set `model.base_url` to a `/v1` endpoint reachable from the harness container.
  Host-loopback `127.0.0.1` refers to the container itself.
- Set `model.id` to the exact served model ID and export the credential under
  `model.api_key_env` (`MODEL_API_KEY` in the example). For an unauthenticated
  local server, use a non-secret placeholder if the server accepts one.
- Keep `harness.type: "codex"` paired with `bridge.type: "codex-ssh"`.
  `harness.codex.executable` is relative to the profile file. Optionally set
  `model.context_length` to the model's actual window.

Use `runtime.backend: "openai"` for an external server, or `"sglang"` for
the existing SGLang runtime path. Both require streaming `/v1/responses` with
tool calls. Model discovery checks `/v1/models`; it does not test Responses
compatibility. This integration has no Chat Completions translation and
rejects the native DeepSeek backend, `max_tokens`, `temperature`, voice, web,
compaction, and subagent configuration overrides.

```sh
./bin/aries setup profiles/codex-tb2-fix-git-openai.json
./bin/aries profiles/codex-tb2-fix-git-openai.json
```

`setup` prepares the benchmark and container images without contacting the
model. During a run, ARIES keeps the model credential in the harness only;
task commands do not receive it. The task image must support the selected
Linux architecture and provide `/bin/sh`, `mkdir`, `chmod`, and `rm` for bridge
staging and cleanup. Native command execution also needs the requested shell.
The bridge stages about 300 MiB of binaries per active task.

Native commands inherit the task image's environment, including toolchain
variables such as `CARGO_HOME` and `RUSTUP_HOME`. ARIES disables login shells
so shell startup does not replace the image's `PATH`; omit `login` or set it
to false in native tool calls. The model-key variable remains explicitly
excluded from native task commands.

## Evidence and validation

Private harness artifacts include `config.toml`, `environments.toml`,
`trajectory.jsonl`, `stderr.log`, and `container.log`. Bridge artifacts
contain replayable native RPC inputs and must remain private. Revocation
removes the task-side binaries, the host SSH identity, and the staged helper;
the public known-hosts entry may remain as evidence.

With the pinned CLI installed, `make integration` includes a real Codex
container, a deterministic local Responses endpoint, and the actual native
SSH executor. Missing CLI prerequisites are reported as a skipped test.
Use `ARIES_CODEX_BINARY` to select another local path to the same pinned
binary. Unit and race tests need neither Docker nor a model service.

The integration is tied to the
[Codex `rust-v0.157.1` source](https://github.com/openai/codex/tree/rust-v0.157.1),
including `environments.toml` and `exec-server --listen stdio`. Review that
native contract before changing the pin. See the
[bridge design](design/bridge.md#codex-ssh-bridge) for descendant cleanup and
the [official configuration reference](https://developers.openai.com/codex/config-reference/)
for custom Responses providers.
