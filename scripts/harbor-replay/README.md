# Harbor source replay

`go build ./cmd/aries-harbor` builds the native vHive runner. With only
`-trajectory` and `-rollout` it inspects an exact reviewed ATIF/native pair.
Its manifest pins both source byte hashes, the task, image digest, default
working directory, and reviewed effects. The native replay verifies the
requested image digest before creating a VM. The operator still needs the
task's original OCI image in the selected containerd namespace.

For the original Git recovery sessions, pass `-ripgrep` with the published
Codex 0.151.0 Linux x64 ripgrep executable. The starting Git object and reflog
checks run during guest setup.

For regex-log and constraints-scheduling, prepare the source tool layout with
Python 3.11+ and the exact Ubuntu Noble amd64 packages recorded under
`packages/`. Ubuntu mirrors or a local package archive must retain the listed
versions. From this directory run:

```sh
python3 prepare-codex-tools.py codex-harbor-tools.tar.gz package-cache
python3 prepare-os-tools.py regex-log regex-log-debs.tar.gz
python3 prepare-os-tools.py constraints-scheduling constraints-scheduling-debs.tar.gz
```

Pass `-codex-tools codex-harbor-tools.tar.gz` and the corresponding
`-os-tools <task>-debs.tar.gz`. The guest installs the checked Ubuntu packages
offline. Guest setup installs the public Node 22.23.2 / Codex 0.151.0 prefix
under its recorded NVM path. Reviewed calls reference the Codex native
executable solely for the recorded local patch operations. The recorded OS
tool version is unavailable. Package versions, initial task checks and setup
time are retained in the log. The synthetic schedule input checks bind all
three original calendar contents. Output includes the final task artifact
digest after the recorded session ends. The original agent's transient
plugins, temporary files and original wall clock can produce differences in
directory lists or dynamic calendar identifiers.

Use `-log` with a new path for each attempt. The `-observer` executable
accepts `phase runtime-id step socket` and blocks the next replay action until
capture ends. The `-sampler` and `-samples` pair starts the one-second host
sampler after restore. The log preserves actual exit status and distinguishes
missing mixed-stream order, source truncation and differing text.
