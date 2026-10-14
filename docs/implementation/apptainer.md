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
internet access but no route to the host's loopback. Runner, the bridge and
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
cannot be changed by that root, and `apt-get` fails. Hermes runs an image
derived with a Docker build, which Apptainer cannot perform; build it on a
Docker host, convert it (`apptainer build hermes.sif docker-archive://hermes.tar`),
and map its tag to the SIF.

An unpacked image keeps none of the OCI image's configuration, but a SIF built
from one does. Setup therefore pulls a registry image into a temporary SIF, and
records the image's working directory beside the unpacked directory; a runtime
whose request names no working directory starts there, as a Docker container
would. An image unpacked without that record is unpacked again.

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

## Limits

- CPU and memory limits need cgroups v2 with delegation, which HPC hosts often
  lack. A request with limits is refused unless `unenforced_limits` accepts them;
  the limits are then recorded but not applied.
- Resource measurement reports unsupported: without cgroups there is no
  per-instance accounting, and sampling `/proc` would lose the CPU time of
  commands that exit between samples.
- Network aliases, mixed backends, concurrency above one and SWE-bench Pro, whose
  commands run as a non-root user, are rejected before any effect.
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

[Provider tests](../../pkg/deployment/apptainer/apptainer_test.go) use a fake
`apptainer`; [real-instance tests](../../pkg/deployment/apptainer/integration_test.go)
run with the `apptainer` build tag on a host that has Apptainer.
