# Docker deployment

ARIES runs on a single Docker Engine with no cluster: the runner starts every
harness and task sandbox as a container through the Moby SDK. This directory
holds the Compose file for the two long-lived pieces you may want as
containers, the tool bridge and the runner itself. For a Kubernetes cluster,
see [`k8s/`](../k8s/README.md) instead.

| Service        | What it is                                                                                  | When you need it                                                   |
| -------------- | ------------------------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| `aries-bridge` | The SSH tool bridge as its own long-running container (`Dockerfile.bridge`)                 | Profiles with `bridge.deployment: "docker"`                        |
| `aries`        | The runner in a container (`Dockerfile`), opt-in via the `runner-container` Compose profile | Hosts that cannot build or run `bin/aries` natively, such as macOS |

Run every command below from the repository root.

## Layouts

**Bridge in the runner (default).** No Compose at all; the bridge is part of
the runner process:

```sh
make build
./bin/aries profiles/hermes-tb2-fix-git-deepseek.json
```

**Bridge in its own container.** The runner stays a process on the host and
talks to the bridge only to grant and revoke, twice per task:

```sh
docker compose -f docker/docker-compose.yml up -d aries-bridge
./bin/aries profiles/hermes-tb2-fix-git-deepseek-docker-bridge.json
```

**Runner in a container too.** Same as above, with the runner started by
Compose instead of from `bin/`:

```sh
docker compose -f docker/docker-compose.yml up -d aries-bridge
PROFILE=profiles/hermes-tb2-fix-git-deepseek-docker-bridge.json \
  docker compose -f docker/docker-compose.yml --profile runner-container run --rm aries
```

`PROFILE` defaults to `profiles/openclaw-tb2-fix-git-deepseek-e2b.json`. The
checked-in profiles for the separate bridge are:

| Profile                                             | Run                                           |
| --------------------------------------------------- | --------------------------------------------- |
| `hermes-tb2-fix-git-deepseek-docker-bridge.json`    | Hermes, one `fix-git` task                    |
| `hermes-tb2-fix-git-x4-deepseek-docker-bridge.json` | Hermes, four `fix-git` tasks at concurrency 4 |
| `openclaw-tb2-fix-git-deepseek-docker-bridge.json`  | OpenClaw over SSH, one `fix-git` task         |

A profile selects the separate bridge with:

```json
"sandbox": { "type": "docker" },
"bridge":  { "type": "hermes-ssh", "deployment": "docker" }
```

Only the SSH bridges (`hermes-ssh`, `openclaw-ssh`) can run separately;
`bridge.advertise_host` and `bridge.namespace` must be unset, and the harness
and sandbox must both be on Docker. The config loader rejects anything else
before the run starts.

## Model key

Both layouts read the DeepSeek key the same way as a host run: from
`DEEPSEEK_API_KEY` in the environment, or from `DEEPSEEK_API.key` in the
working directory. The containerised runner mounts the repository-root
`DEEPSEEK_API.key` read-only, so the key never appears in the container's
environment or `docker inspect` output:

```sh
umask 077
printf '%s' "$DEEPSEEK_API_KEY" > DEEPSEEK_API.key
```

The file is gitignored and excluded from every image by `.dockerignore`. The
bridge container never sees a model key.

## Building the images

`docker compose ... build` builds both from the repository root. The bridge
image is also published as `jingxiang212/aries-bridge:latest`, which Compose
pulls when you have not built it. To build or push it yourself:

```sh
make image-bridge BRIDGE_IMAGE=jingxiang212/aries-bridge:latest
```

`Dockerfile` cross-compiles on the host's native architecture, so building the
runner image on Apple Silicon does not emulate the Go compiler.

## How the separate bridge works

- **Finding it.** The runner looks for exactly one running container labelled
  `aries.component=tool-bridge`. None, or more than one, fails the run before
  any task starts.
- **Control.** Grant and revoke are `docker exec aries-bridge ctl` calls made
  through the Moby SDK; nothing is on the tool-call path and no port is
  published.
- **Per-task isolation.** At grant the runner attaches the bridge to that
  task's network, and the grant listens only on the bridge's address there, so
  a harness can reach its own task's grant and no other. After revocation the
  runner detaches it again so the task network can be removed.
- **Sandbox binding.** The bridge re-reads the task container named in the
  grant and refuses it unless it is a live ARIES task container of that run
  and task, on its own task network, with the expected workdir.
- **Keys and logs.** The runner keeps the SSH client private key and sends only
  the public half. Tool-call logs come back into the run directory at mode 0600
  after revocation, then are deleted from the bridge.
- **Restarts.** A restarted bridge holds no grants. The runner treats that as
  proven revocation, but the tool-call logs of tasks in flight are lost, so
  their evaluation is blocked. `restart: unless-stopped` brings the bridge back
  for the next task.

The design and security argument are in
[`docs/design/bridge.md`](../docs/design/bridge.md#serving-from-a-separate-bridge).

## Hardening

The bridge container runs with a read-only root filesystem, tmpfs state, all
capabilities dropped and `no-new-privileges`. It runs as root only because it
must open the Docker socket, which is root-equivalent on the host; the runner
holds the same socket.

## Checking and cleaning up

```sh
docker compose -f docker/docker-compose.yml ps
docker exec aries-bridge aries-bridge status
docker ps -a --filter label=aries.managed
docker network ls --filter label=aries.managed
docker compose -f docker/docker-compose.yml down
```

`status` reports the grants the bridge currently holds; between runs it should
be zero, and no `aries.managed` containers or networks should remain.
