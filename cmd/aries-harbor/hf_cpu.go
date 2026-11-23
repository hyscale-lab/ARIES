package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
)

//go:embed hf_cpu_host.py
var hfCPUHost string

const hfCPUGuest = `import ast, json, os, sys
from pathlib import Path
phase = sys.argv[1]
cpus = os.cpu_count()
affinity = sorted(os.sched_getaffinity(0))
data = {"guest_logical_cpus":cpus, "guest_allowed_cpus":affinity,
        "guest_cpu_online": Path("/sys/devices/system/cpu/online").read_text().strip()}
assert cpus == len(affinity) == 2 and "PYTHON_CPU_COUNT" not in os.environ
if phase == "after_source":
 import torch
 source = ast.parse(Path("/app/app.py").read_text())
 selections = [s for s in source.body if isinstance(s, ast.Expr) and isinstance(s.value, ast.Call) and ast.unparse(s.value.func) == "torch.set_num_threads"]
 assert len(selections) == 1
 statement = ast.unparse(selections[0])
 assert statement == "torch.set_num_threads(min(2, os.cpu_count() or 1))"
 torch.set_num_threads(min(2, os.cpu_count() or 1))
 data.update(original_thread_selection=statement, probe_torch_threads=torch.get_num_threads(), servers=[])
 assert data["probe_torch_threads"] == 2
 for p in Path("/proc").glob("[0-9]*"):
  try:
   args=(p/"cmdline").read_bytes().split(b"\0")
   if not any(a in (b"app.py", b"/app/app.py") for a in args): continue
   data["servers"].append({"pid":int(p.name),"argv":[a.decode() for a in args if a],
       "threads":[(t/"comm").read_text().strip() for t in (p/"task").iterdir()]})
  except (FileNotFoundError, ProcessLookupError): pass
 assert len(data["servers"]) == 1, data
print(json.dumps(data))`

func checkHFCPU(ctx context.Context, m *vhive.Manager, id string, hostCPU int, phase string, log *json.Encoder) error {
	socket, err := m.VMMSocket(ctx, id)
	if err != nil {
		return err
	}
	host, err := exec.CommandContext(ctx, "python3", "-I", "-c", hfCPUHost, socket, strconv.Itoa(hostCPU)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("host vCPU check: %w: %s", err, host)
	}
	guest, err := checked(ctx, m, id, core.Command{Path: "/usr/local/bin/python3", Args: []string{"-I", "-c", hfCPUGuest, phase}, Dir: "/app"})
	if err != nil {
		return err
	}
	return log.Encode(map[string]any{"event": "diagnostic_hf_cpu_readback", "phase": phase,
		"host": json.RawMessage(host), "guest": json.RawMessage(guest)})
}
