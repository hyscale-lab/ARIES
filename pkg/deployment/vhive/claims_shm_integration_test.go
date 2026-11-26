package vhive

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

// This loopback-only fixture uses generated content and leaves Chromium closed
// at the saved boundary. The writer and both readers measure their own tmpfs.
const claimsMountProbe = `
const fs = require('node:fs');
const http = require('node:http');
const {execFileSync} = require('node:child_process');
const path = require('node:path');
const stat = fs.statfsSync('/dev/shm');
const marker = '/dev/shm/aries-browser-smoke';
if (process.getuid() !== 1001) throw Error('expected published Playwright UID');
fs.writeFileSync(marker, 'aries-smoke', {flag:'wx'});
const globalModules = execFileSync('npm', ['root', '-g'], {encoding:'utf8'}).trim();
const mcp = path.join(globalModules, '@playwright/mcp');
const {chromium} = require(require.resolve('playwright', {paths:[mcp]}));
http.createServer(async (req, res) => {
  if (req.method !== 'GET' || req.url !== '/mount-size') { res.writeHead(404).end(); return; }
  const report = {shm_bytes:stat.bsize * stat.blocks, uid:process.getuid(), marker:fs.readFileSync(marker,'utf8')};
  try {
    const browser = await chromium.launch({timeout:8000, ignoreDefaultArgs:['--disable-dev-shm-usage']});
    try {
      const page = await browser.newPage();
      await page.setContent('<title>ARIES shared-memory smoke</title><p>generated fixture</p>', {timeout:8000});
      report.chromium_title = await page.title();
    } finally {
      await browser.close();
    }
  } catch (error) {
    report.chromium_error = String(error).slice(0,1024);
  }
  res.writeHead(200, {'content-type':'application/json'}).end(JSON.stringify(report));
}).listen(18074, '127.0.0.1');
`

const claimsDefaultProbe = `
from http.server import BaseHTTPRequestHandler, HTTPServer
import json, os
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        stat = os.statvfs('/dev/shm')
        output = {'shm_bytes': stat.f_frsize * stat.f_blocks, 'marker_absent': not os.path.exists('/dev/shm/aries-browser-smoke')}
        body = json.dumps(output).encode()
        self.send_response(200); self.end_headers(); self.wfile.write(body)
HTTPServer(('127.0.0.1', 18073), Handler).serve_forever()
`

const claimsMainProbe = `
import http.client, json, os
from pathlib import Path

def check(port, path):
    conn = http.client.HTTPConnection('127.0.0.1', port, timeout=15)
    try:
        conn.request('GET', path)
        response = conn.getresponse()
        body = response.read(3072).decode()
        return {'status': response.status, 'data': json.loads(body) if response.status == 200 else body}
    finally:
        conn.close()

interfaces = sorted(os.listdir('/sys/class/net'))
routes = [row.split()[0] for row in Path('/proc/net/route').read_text().splitlines()[1:] if row.split()]
stat = os.statvfs('/dev/shm')
report = {'main_exec_uid': os.geteuid(), 'interfaces': interfaces, 'route_devices': routes,
          'main_shm_bytes': stat.f_frsize * stat.f_blocks,
          'main_marker_absent': not os.path.exists('/dev/shm/aries-browser-smoke'),
          'default_companion': check(18073, '/probe'), 'playwright_mount_probe': check(18074, '/mount-size')}
print(json.dumps(report, sort_keys=True))
assert report['main_exec_uid'] == 65534 and interfaces == ['lo'] and routes == []
assert report['main_shm_bytes'] == 67108864 and report['main_marker_absent']
assert report['default_companion'] == {'status': 200, 'data': {'shm_bytes': 67108864, 'marker_absent': True}}
assert report['playwright_mount_probe'] == {'status': 200, 'data': {'shm_bytes': 1073741824, 'uid': 1001, 'marker': 'aries-smoke', 'chromium_title': 'ARIES shared-memory smoke'}}
`

func TestClaimsShm(t *testing.T) {
	stage := os.Getenv("ZB_CLAIMS_SHM_STAGE")
	if stage == "" {
		t.Skip("opt-in published fixture partial readiness")
	}
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	inputs, err := os.ReadFile(filepath.Join(stage, "images.tsv"))
	require(err)
	images := map[string]string{}
	for _, row := range strings.Split(strings.TrimSpace(string(inputs)), "\n") {
		key, value, ok := strings.Cut(row, "\t")
		if !ok || !strings.Contains(value, "@sha256:") {
			t.Fatal("invalid public image fixture")
		}
		images[key] = value
	}
	if len(images) != 2 {
		t.Fatal("fixture requires the two pinned published images")
	}
	const runtime = "/local/zb-v5-matched-deep/runtime"
	const unit = "/run/fcmod-zepbound-amd183-v5-deep"
	const namespace = "aries-claims-shm-20261126"
	vm, err := New(Options{Socket: unit + "/containerd.sock", Namespace: namespace, CheckpointDir: runtime + "/checkpoints",
		ShimDir: runtime + "/shim", IODir: unit + "/aries-io", IdleMode: "checkpoint", GuestTaskV3: true,
		NativeCheckpoint: true, VCPUs: 2, MemoryMB: 4096, Companions: []CompanionTask{
			{Role: "default-shm", Image: images["main"], Command: []string{"/usr/local/bin/python3", "-c", claimsDefaultProbe}},
			{Role: "playwright-mcp", ShmSizeMiB: 1024, Image: images["playwright-mcp"], Command: []string{"/bin/sh", "-c", "exec node -e \"$0\"", claimsMountProbe}},
		}})
	require(err)
	t.Cleanup(func() { require(vm.Close()) })
	run, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	ctx := vm.ctx(run)
	id := fmt.Sprintf("zb-claims-shm-%d", time.Now().UnixNano())
	ids := []string{id, id + "-default-shm", id + "-playwright-mcp"}
	log, err := os.OpenFile(filepath.Join(stage, "events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	require(err)
	t.Cleanup(func() { require(log.Close()) })
	enc := json.NewEncoder(log)
	record := func(name string, value any) {
		at, err := harbor.Clock()
		require(err)
		require(enc.Encode(map[string]any{"event": name, "monotonic_ns": at, "vm_id": id, "data": value}))
	}
	request := deployment.Request{Name: id, Image: images["main"], Workdir: "/app", StorageMB: 10240, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"},
		Labels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": id, "aries.task": "claims-published-shm-smoke"}}
	record("create_start", map[string]any{"images": images, "vcpus": 2, "memory_mib": 4096, "storage_mib": 10240, "playwright_public_entrypoint": false, "network_devices": 0, "playwright_shm_mib": 1024})
	actual, err := vm.Create(ctx, request)
	if actual != "" {
		t.Cleanup(func() {
			clean, done := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer done()
			require(vm.Stop(clean, actual))
			requireThreeAllocationsAbsent(t, clean, vm, ids)
			record("removed", ids)
		})
	}
	require(err)
	if actual != id {
		t.Fatal("manager returned a different VM identity")
	}
	require(vm.Validate(ctx, id, request, nil))
	health := func() json.RawMessage {
		var observation string
		for attempts := 0; attempts < 8; attempts++ {
			out, err := vm.Exec(ctx, id, core.Command{Path: "/usr/local/bin/python3", Args: []string{"-c", claimsMainProbe}, User: "65534:65534", Timeout: 20 * time.Second, OutputLimitBytes: 4096})
			if err == nil && out.ExitCode == 0 && json.Valid([]byte(out.Stdout)) {
				return json.RawMessage(strings.TrimSpace(out.Stdout))
			}
			observation = fmt.Sprintf("error=%v exit=%d stdout=%s stderr=%s", err, out.ExitCode, out.Stdout, out.Stderr)
			t.Log(observation)
			time.Sleep(time.Second)
		}
		t.Fatalf("unprivileged mount or Chromium readback failed: %s", observation)
		return nil
	}
	beforeHealth := health()
	before := readThreeNativeTasks(t, ctx, vm, id, ids)
	beforeVMM, err := vm.checkpointProcesses(ctx, id)
	require(err)
	record("ready", map[string]any{"task_state": before, "main_readback": beforeHealth})
	record("snapshot_start", nil)
	require(vm.Suspend(ctx, id))
	require(confirmCheckpointProcesses(beforeVMM))
	state, err := vm.loadCheckpoint(id)
	require(err)
	manifest, err := os.ReadFile(filepath.Join(state.NativeDirectory, "manifest.json"))
	require(err)
	var saved struct {
		Tasks     []struct{ ID string }
		Processes []struct {
			TaskID, ExecID string
			State          struct{ Pid uint32 }
		}
		Networks []json.RawMessage
	}
	require(json.Unmarshal(manifest, &saved))
	if len(saved.Networks) != 0 || len(saved.Tasks) != 3 || len(saved.Processes) != 3 {
		t.Fatal("checkpoint tasks or network differs")
	}
	var captured []string
	for _, task := range saved.Tasks {
		captured = append(captured, task.ID)
	}
	expected := append([]string{}, ids...)
	sort.Strings(captured)
	sort.Strings(expected)
	if !reflect.DeepEqual(captured, expected) {
		t.Fatal("checkpoint captured different tasks")
	}
	savedPIDs := map[string]uint32{}
	for _, p := range saved.Processes {
		if p.ExecID != "" || p.State.Pid == 0 {
			t.Fatal("checkpoint contains an unexpected exec")
		}
		savedPIDs[p.TaskID] = p.State.Pid
	}
	for _, p := range before {
		if p.PID != savedPIDs[p.ID] {
			t.Fatal("checkpoint task PID differs")
		}
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(manifest))
	record("snapshot_complete", map[string]any{"manifest_sha256": digest, "task_ids": captured, "init_pids": savedPIDs, "network_devices": len(saved.Networks), "original_vmm_absent": true})
	record("restore_start", nil)
	require(vm.Resume(ctx, id))
	after := readThreeNativeTasks(t, ctx, vm, id, ids)
	afterHealth := health()
	afterVMM, err := vm.checkpointProcesses(ctx, id)
	require(err)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeHealth, afterHealth) || beforeVMM[0] == afterVMM[0] {
		t.Fatal("restore task identity or sanitized readback differs")
	}
	retained, err := os.ReadFile(filepath.Join(state.NativeDirectory, "manifest.json"))
	require(err)
	if fmt.Sprintf("%x", sha256.Sum256(retained)) != digest {
		t.Fatal("checkpoint manifest changed")
	}
	record("restore_complete", map[string]any{"task_state": after, "main_readback": afterHealth, "manifest_sha256": digest, "host_vmm_replaced": true})
}
