# Recovered native validation helpers

These historical source snapshots came from
`amd205.utah.cloudlab.us:/home/nehalem/aries/runs/tsb-access-dirty-001/`.
Directory names preserve their original locations. `SOURCE.json` records exact
origins, source hashes and omitted duplicates; `SHA256SUMS` verifies the retained
bytes. Recovery checked Python syntax and credential literals without executing
the helpers or collecting runtime outputs.

- `idle-truth/` contains a controlled guest marker and host fixture comparing
  known reads, writes, restored writes and an untouched page with idle tracking
  and VMM dirty logs. Its original sibling `idle.py` exactly matches the retained
  `scripts/tsb-pages/idle.py` and is not duplicated here.
- `native-harness-vmm-001/` contains local mock model/MCP servers and the ARIES
  native fixture launcher. The mock API key is a fixed test sentinel.
- `rerun-lifecycle-native-fixtures-002/` contains the runtime-selection launcher
  and distinct normal/deadline fixture drivers. Both cases originally used the
  MCP server retained in `native-harness-vmm-001/mock-mcp.py`; both used the model
  server retained in `normal/mock-model.py`. Identical copies are omitted.

These files retain historical worker paths and are not a portable fixture
package. They refer to `/home/nehalem/aries`, built ARIES/Firecracker binaries,
the guest kernel/initrd, generated profile and attribution configs, and runtime
selection/preparation JSON. The lifecycle launcher also expects the original
`normal/` and `paused-deadline/` preparation directories and temporarily selects
`/etc/containerd/firecracker-runtime.json`. Those generated inputs, binaries,
captures and results were not recovered into this source directory.

The current observer tests remain under `scripts/tsb-pages/` and
`scripts/tsb-attribution/`. These helpers are retained because their complete
source is absent from the committed tests, including their embedded scripts;
their presence is not evidence that native validation was rerun.
