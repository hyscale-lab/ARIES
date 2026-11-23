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

const hfDefaultThreadsTrial = "b615a3ec-fae8-4198-ae4c-0aedf9c3e25b"

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
if sys.argv[2] == "b615a3ec-fae8-4198-ae4c-0aedf9c3e25b":
 data["guest_topology"]={p.name:(p/"topology/thread_siblings_list").read_text().strip() for p in Path("/sys/devices/system/cpu").glob("cpu[0-9]*")}
 data["thread_environment"]={k:os.environ.get(k) for k in ("OMP_NUM_THREADS","MKL_NUM_THREADS","PYTHON_CPU_COUNT")}
 if sys.argv[3] == "true":
  assert set(data["guest_topology"].values()) == {"0-1"}, data
  assert all(value is None for value in data["thread_environment"].values()),data
  # The original step 6 imports Torch and reads its default. Preserve the
  # pre-source memory state by reading only kernel CPU topology before then.
  if phase != "after_source":
   assert "torch" not in sys.modules
   data["torch_probe"]="deferred_to_original_step6"
   print(json.dumps(data)); sys.exit(0)
 import torch
 data["probe_torch_default_threads"]=torch.get_num_threads()
 if sys.argv[3] == "true": assert data["probe_torch_default_threads"] == 1,data
 if phase=="after_source":
  app=ast.parse(Path("/app/app.py").read_text())
  setters=[ast.unparse(x.func) for x in ast.walk(app) if isinstance(x,ast.Call) and ast.unparse(x.func).endswith("set_num_threads")]
  assert not setters
  data["source_app_thread_setters"]=setters
  from transformers import AutoModelForSequenceClassification, AutoTokenizer
  model_path="/app/model_cache/sentiment_model"
  tokenizer=AutoTokenizer.from_pretrained(model_path,local_files_only=True)
  model=AutoModelForSequenceClassification.from_pretrained(model_path,local_files_only=True)
  model.eval()
  with torch.inference_mode():
   tokens=tokenizer("I absolutely love this!",return_tensors="pt",truncation=True)
   probs=torch.softmax(model(**tokens).logits,dim=-1)[0].tolist()
  data["offline_prediction"]={model.config.id2label[i].lower():v for i,v in enumerate(probs)}
  data["visible_cpus_after_prediction"]=os.cpu_count()
  data["torch_threads_after_prediction"]=torch.get_num_threads()
  assert data["offline_prediction"]["positive"]>0.99
  assert data["visible_cpus_after_prediction"]==cpus and data["torch_threads_after_prediction"]==data["probe_torch_default_threads"]
 print(json.dumps(data))
 sys.exit(0)
if phase == "after_source":
 import hashlib
 import torch
 model=Path("/app/model_cache/sentiment_model")
 expected_files={"README.md":"75f17282923fea5b541df92640e2d8646c4ff5e8e3e16ed0a7e4e50ee7588851",
    "config.json":"582122c8f414793d131e10022ce9ba04e3811a9da6389137ee2f18665b4f4d15",
    "model.safetensors":"7c3919835e442510166d267fe7cbe847e0c51cd26d9ba07b89a57b952b49b8aa",
    "tokenizer_config.json":"5ab9097b4149371c5fd52b2d6e26cb6f9c07c0d19fcfdda895b1adad6b57c3e0",
    "vocab.txt":"07eced375cec144d27c900241f3e339478dec958f92fddbc551f295c992038a3"}
 assert {p.name for p in model.iterdir() if p.is_file()} == set(expected_files)
 model_files={}
 for name in expected_files:
  with (model/name).open("rb") as f: model_files[name]=hashlib.file_digest(f,"sha256").hexdigest()
 assert model_files == expected_files
 data["model_file_sha256"]=model_files
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
 server = data["servers"][0]
 p = Path("/proc") / str(server["pid"])
 server["affinity"] = sorted(os.sched_getaffinity(server["pid"]))
 server["cpu_count_override"] = any(v.startswith(b"PYTHON_CPU_COUNT=") for v in (p/"environ").read_bytes().split(b"\0"))
 server["no_python_options"] = server["argv"] == ["python", "-u", "/app/app.py"]
 assert server["affinity"] == affinity and not server["cpu_count_override"] and server["no_python_options"]
 # Exercise the still-running original server after its source session, using one
 # request at a time. The thread counters describe the actual endpoint process.
 import requests
 def thread_cpu():
  return {int(t.name):int((t/"schedstat").read_text().split()[0]) for t in (p/"task").iterdir()}
 before=thread_cpu()
 predictions=[]
 for _ in range(4):
  result=requests.post("http://127.0.0.1:5000/sentiment",json={"text":"I absolutely love this!"},timeout=10)
  result.raise_for_status()
  predictions.append(result.json()["sentiment"])
 after=thread_cpu()
 assert predictions==["positive"]*4
 server["serial_prediction_thread_cpu_ns"]={str(t):after[t]-before.get(t,0) for t in after}
 server["serial_predictions"]=predictions
print(json.dumps(data))`

func checkHFCPU(ctx context.Context, m *vhive.Manager, id, trial string, hostCPU int, sourceCPU bool, phase string, log *json.Encoder) error {
	socket, err := m.VMMSocket(ctx, id)
	if err != nil {
		return err
	}
	host, err := exec.CommandContext(ctx, "python3", "-I", "-c", hfCPUHost, socket, strconv.Itoa(hostCPU)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("host vCPU check: %w: %s", err, host)
	}
	guest, err := checked(ctx, m, id, core.Command{Path: "/usr/local/bin/python3", Args: []string{"-I", "-c", hfCPUGuest, phase, trial, strconv.FormatBool(sourceCPU)}, Dir: "/app"})
	if err != nil {
		return err
	}
	event := "diagnostic_hf_cpu_readback"
	if sourceCPU {
		event = "source_hf_cpu_readback"
	}
	return log.Encode(map[string]any{"event": event, "phase": phase,
		"host": json.RawMessage(host), "guest": json.RawMessage(guest)})
}
