# Apptainer deployment

[`pkg/deployment/apptainer`](../../pkg/deployment/apptainer/apptainer.go) places
harnesses and sandboxes in [Apptainer](https://apptainer.org) instances, for
hosts where a normal user cannot run Docker, such as HPC clusters. It drives the
`apptainer` binary, needs no daemon and no root, and requires Apptainer 1.5 or
newer. Selection, configuration and the run namespace are wired in
[`cmd/aries`](../../cmd/aries/wiring.go) and
[deployment wiring](../../internal/app/wiring/deployment/apptainer.go); the
profile fields are in the [configuration reference](../configuration.md#apptainer-placement).

## Composition and topology

Harness, sandbox and bridge all select `apptainer` with one shared
configuration. A normal user cannot give an Apptainer instance its own network,
so `aries PROFILE` first re-executes itself in a new user and network namespace
that maps the invoking user to itself. `slirp4netns` gives the namespace
internet access but no route to the host's loopback. It exits with the `aries`
process outside the namespace, which alone holds its exit pipe, even when that
process is killed. Runner, the bridge and
every instance share the loopback of the namespace; nothing in it can be reached from
outside, which keeps the unauthenticated control and SSH ports private on a host
shared with other users. Services are addressed at `127.0.0.1`, so a run
executes one task at a time (`execution.concurrency: 1`). Instances resolve
names through the forwarder of `slirp4netns`; Runner uses the host's resolver
configuration. The recorded network policy is `run-namespace`. `aries setup`
does not enter a namespace.

The bridge runs `aries-bridge`, built beside `aries`, as a plain process in the
namespace. Apptainer cannot enter an instance's user namespace from inside
another instance, so a containerized bridge could not execute in the sandboxes.
The bridge's paths are host paths in a private directory under `state_dir`; it
receives only the environment needed to run `apptainer`, not Runner's
credentials. It reads the sandboxes' ownership records in `state_dir` before it
executes in them.

## Images

`aries setup` unpacks each pinned image into a directory under `image_dir`
(`apptainer build --fix-perms --sandbox`), from its registry or from a SIF that
`images` maps it to. An unprivileged build gives every file to the invoking
user, which is root inside the instance. Run from a SIF instead, files that
belong to other users in the image, such as the partial-download directories of `apt`,
cannot be changed by that root, and `apt-get` fails.

Only Hermes's image needs a step of your own. ARIES derives it with a Docker
build, which Apptainer cannot perform, so it is built on a host with Docker,
saved to a file and converted to a SIF. From a clone of this repository on that
host:

```bash
make build
bin/aries setup profiles/hermes-tb2-fix-git-deepseek.json
TAG=$(grep -o -m 1 'aries-local/hermes:[^"]*' profiles/apptainer/hermes-tb2-fix-git-apptainer.json)
docker save "$TAG" -o hermes.tar
apptainer build hermes.sif docker-archive:hermes.tar
```

Setup with a Hermes profile on Docker builds the image, and calls no model. The
last command needs Apptainer, and can run on the cluster instead. Then map the
tag to the SIF in the `images` of the harness, sandbox and bridge placements, as
the [example profile](../../profiles/apptainer/hermes-tb2-fix-git-apptainer.json)
does:

```json
"images": { "aries-local/hermes:TAG": "/scratch/users/me/aries/hermes.sif" }
```

The [PBS example](#running-on-a-pbs-cluster) converts the file and maps the tag
itself.

The rest happens in setup, with nothing for you to do. A directory unpacked from
a registry image loses the image's configuration, including its working
directory, but a SIF keeps it. Setup therefore pulls a registry image into a
temporary SIF first, and records the working directory beside the unpacked
directory; a runtime whose request names no working directory starts there, as a
Docker container would. Setup unpacks again any image that lacks this record.

## Runtime creation and execution

`Create` starts an empty instance of the unpacked image under `--containall
--fakeroot`, with its own writable overlay: a directory, or a sparse fixed-size
overlay image when the request bounds storage, which enforces the bound.
`--containall` hides the host's home directory, `/tmp` and environment. `Start`
runs the requested entrypoint, or the image's runscript, inside the instance, so
private files can be staged between `Create` and `Start` as with Docker.
Apptainer has no labels; a private record per runtime holds ownership,
configuration and the instance's PID, which with the instance name is its
identity.

Commands keep their exact argv. A small shell in the instance runs each one and
reports its exit status in a trailer on stderr, since apptainer's own failures
also exit nonzero. As with Docker and Kubernetes, cancellation stops the local
`apptainer exec` and may leave the command running in the instance; `Stop`
removes what remains. Environment values travel in owner-only files under a
read-only bind at `/.aries`, read by the instance's own shell: on apptainer's
command line they would be visible to every user of the host, and Apptainer's
`--env-file` does not preserve every value. Files move as tar streams through
`apptainer exec`.

Every command runs as the instance's root. Without subordinate IDs in
`/etc/subuid`, that root is the invoking user's own ID, and a command for any
other user is refused; Hermes is told to keep root rather than drop to its own
user. Package managers need more than one ID. When a `fakeroot-sysv` command is
on `PATH` with `faked-sysv` beside it, Apptainer injects it into every command
of the instance and `apt-get install` works. Its library must be built against
a glibc no newer than the task images': a build for an old distribution, such
as EPEL 8's `fakeroot` (glibc 2.28), unpacked into a user directory, suits
Debian- and Ubuntu-based images; a newer host build makes every command of an
older image fail.

Each `apptainer exec` starts its own `faked`, which ends with the command. Hermes
saves a command's environment and restores it in the next, so that command would
send to a finished daemon's queue until the queue filled and the command blocked.
`Create` therefore starts one `faked` for the instance and writes its key into
the runtime's environment, so every command uses the same daemon, and ownership
that a command fakes stays visible to later ones. A command that kills this
daemon stalls later commands until they time out.

## Resource measurement

Without cgroups v2 an unprivileged user has no cgroup per instance, so the
resource source measures each runtime from `/proc` at every sample. An
instance's CPU is the time of every live process in its PID namespace,
including the children they have reaped, and of its starter's storage helpers.
An instance in the host's PID namespace, as when an administrator disables PID
namespaces, cannot be told apart from other processes, and its samples fail.
A command that ends between two samples is still counted: as it ends, its
wrapper reads the CPU the command used from its own `/proc` entry and reports
it in the exit trailer, and the provider adds it to the runtime's total. A
process that a command leaves running is reaped by the instance's init, whose
time then includes it. Memory is the processes' proportional set size, so a
page they share counts once; unlike a cgroup's usage, it excludes the page
cache. The reported memory limit is the host's memory. As with Docker,
apptainer's own launcher, about 0.06 CPU-seconds per command outside the
instance, and archive transfers are not charged to the instance. The bridge is
measured by its own process; the commands it runs count for the sandbox that
runs them. The [resource tests](../../pkg/deployment/apptainer/resources_integration_test.go)
run commands shorter than a sample, and a process left behind, and check the
measurement against the CPU each of them reports for itself.

## Limits

- CPU and memory limits need cgroups v2 with delegation, which HPC hosts often
  lack. A request with limits is refused unless `unenforced_limits` accepts them;
  the limits are then recorded but not applied.
- Resources are measured from processes, not from a cgroup: memory excludes the
  page cache, and CPU time used by Apptainer's launcher outside the instance is
  not counted.
- Network aliases, mixed backends, concurrency above one and SWE-bench Pro, whose
  commands run as a non-root user, are rejected before any effect. So is the
  `hermes-grpc` bridge, whose typed file operations are not implemented here; the
  SSH bridges need only exec.
- Ubuntu 24.04 restricts unprivileged user namespaces through AppArmor
  (`kernel.apparmor_restrict_unprivileged_userns`); with the restriction on,
  Apptainer cannot start a fakeroot instance inside the run namespace. On such
  a host Runner's own name lookups also fail inside the namespace when the host
  resolves through `127.0.0.53`, so endpoints Runner contacts must be addresses.
- Toolathlon's application-backed tasks need Docker on the host and are not
  supported.

## Removal

`Stop` ends the started command's process group, stops the instance, confirms
with `apptainer instance list` that it is gone, and removes the overlay and the
record. An instance with no record, or whose PID differs from its record, is
never stopped. The bridge process is stopped by process group and its directory
removed. The namespace ends with the run's last process.

As on the other backends, a Runner that crashes leaves its runtimes behind, and
the bridge process keeps running, which also keeps the namespace. Every
runtime's `Stop` works from its record on disk, so a later process removes them;
a PID that now names another process is not signaled. `aries cleanup` finds the
run's instances and bridge by their records in `state_dir` and stops them, the
bridge last (see [after a crash](../run-results.md#after-a-crash)).

## Running on a PBS cluster

[`scripts/apptainer-example.pbs`](../../scripts/apptainer-example.pbs) is a job to start from, written
and tested on NSCC's ASPIRE 2A. One GPU node serves Qwen3-4B with SGLang and runs Hermes on
Terminal-Bench 2 tasks, one at a time, with the whole of ARIES on that node. The job runs from a clone of
this repository: it builds ARIES from the clone and uses its example profile and pinned versions, so the
script alone is not enough.

1. On a host with Docker, save Hermes's image with the commands under [Images](#images), up to `docker save`.
2. On the cluster's login node, clone the repository on scratch and make its cache directory:

   ```bash
   git clone https://github.com/hyscale-lab/ARIES.git
   cd ARIES
   mkdir -p .cache
   ```

3. Copy `hermes.tar` from the Docker host to `.cache/hermes.tar` in that clone, with `scp` or `rsync`.
4. Submit the two jobs from the clone, with your project in place of `<project>`:

   ```bash
   setup=$(qsub -P <project> -v SETUP_ONLY=1 -l select=1:ncpus=16:mem=96gb -l walltime=08:00:00 scripts/apptainer-example.pbs)
   qsub -P <project> -W depend=afterok:$setup scripts/apptainer-example.pbs
   ```

The first job needs no GPU. It converts `.cache/hermes.tar` to `.cache/hermes.sif`, builds ARIES, unpacks
`fakeroot`, pulls the SGLang image, downloads the model's weights and runs `aries setup`, skipping whatever is
already done. Its walltime keeps it out of NSCC's `qdev` queue, whose limit is two hours. The second waits for
the first and runs `fix-git`; `-v TASKS=fix-git:vulnerable-secret` chooses other tasks. Nothing needs mapping:
each job writes its own profile, `runs/example-<job>/profile.json`, with Hermes's tag mapped to
`.cache/hermes.sif`. Each job's logs and results are in `runs/example-<job>/`, as
[run results](../run-results.md) describes. `.cache` grows to about 52 GB.

On 6 November 2026 the first job took 34 minutes from a fresh clone, most of it for the SGLang image and the
weights; converting `hermes.tar` added 71 seconds on 9 November. A GPU job with three tasks took 17 minutes
once it started; the wait for a GPU that day ran from seconds to nearly an hour.

Expect a score of 0. Qwen3-4B failed `fix-git`, `vulnerable-secret` and `openssl-selfsigned-cert`, and those
tasks' evaluations report `failed`. Each task's `evaluation/ctrf.json` holds the grader's test results; a
task without them was not graded, which points at the run and not at the model. `fix-git`'s own solution,
sent through Hermes by a scripted model on a CPU node, scored 1.

## Tests

[Provider tests](../../pkg/deployment/apptainer/apptainer_test.go) use a fake
`apptainer`. [Real-instance tests](../../pkg/deployment/apptainer/integration_test.go)
need a host with Apptainer and an image, and `make integration` does not run them:

```bash
make integration-apptainer ARIES_APPTAINER_TEST_IMAGE=/path/to/image.sif
```

The fakeroot test is skipped unless `fakeroot-sysv` is on `PATH`. On Ubuntu 24.04
the tests also need the AppArmor restriction described under [Limits](#limits) off.
