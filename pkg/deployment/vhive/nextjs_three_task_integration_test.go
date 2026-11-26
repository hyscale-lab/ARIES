package vhive

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	taskv3 "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/errdefs"
	"github.com/containerd/ttrpc"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

const thirdTaskHealth = `
const http = require('node:http');
const nonce = require('node:crypto').randomUUID();
http.createServer((req, res) => {
  if (req.method !== 'GET' || req.url !== '/__health') {
    res.writeHead(404).end();
    return;
  }
  res.writeHead(200, {'content-type': 'application/json'}).end(JSON.stringify({ok:true, nonce}));
}).listen(4102, '127.0.0.1');
`

const mainTaskHealth = `
const assert = require('node:assert/strict');
const fs = require('node:fs');
(async () => {
  assert.deepEqual(fs.readdirSync('/sys/class/net'), ['lo']);
  const health = {};
  for (const port of [4100, 4101, 4102]) {
    const r = await fetch('http://127.0.0.1:' + port + '/__health', {redirect:'error', signal:AbortSignal.timeout(250)});
    assert.equal(r.status, 200);
    const body = await r.json();
    assert.equal(body.ok, true);
    if (port === 4102) assert.match(body.nonce, /^[0-9a-f-]{36}$/);
    health[port] = {status:r.status, body};
  }
  console.log(JSON.stringify({interfaces:['lo'], health}));
})().catch(error => { console.error(error); process.exitCode=1; });
`

type nativeHealthTask struct {
	ID     string
	PID    uint32
	Status string
}

// Opt-in, using only the pinned and already imported Next.js fixture images.
func TestNativeNextJSThreeTasks(t *testing.T) {
	stage := os.Getenv("ZB_NEXTJS_THREE_STAGE")
	if stage == "" {
		t.Skip("requires the isolated amd191 Next.js fixture")
	}
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const fixture = "/local/zb-nextjs-be1d1d11/runtime"
	const unit = "/run/fcmod-zepbound-nextjs191"
	const app = "localhost/zb-nextjs-runtime@sha256:3d877267cf0f0d27a4e7157cd3e7c42ea2e1b3787e4d790bc171b6311234dee4"
	const warehouse = "localhost/zb-nextjs-warehouse@sha256:decefdfc46bbed17ebdc58a58d0b220ae52d4c71b363ce42b58ecde17d309ba8"
	deadline, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	options := Options{Socket: unit + "/containerd.sock", Namespace: "aries",
		CheckpointDir: fixture + "/checkpoints", ShimDir: fixture + "/shim",
		IODir: unit + "/aries-io", IdleMode: "checkpoint", GuestTaskV3: true,
		NativeCheckpoint: true, VCPUs: 2, MemoryMB: 4096,
		Companions: []CompanionTask{
			{Role: "warehouse", Image: warehouse},
			{Role: "health", Image: app, Command: []string{"/usr/local/bin/node", "-e", thirdTaskHealth}},
		}}
	m, err := New(options)
	require(err)
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := m.ctx(deadline)
	id := fmt.Sprintf("zb-nextjs-three-%d", time.Now().UnixNano())
	ids := []string{id, id + "-warehouse", id + "-health"}
	req := deployment.Request{Name: id, Image: app, Workdir: "/app", StorageMB: 15360, Entrypoint: []string{"/bin/sleep"},
		Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.kind": "task-container",
			"aries.component": "sandbox", "aries.run": id, "aries.task": "nextjs-native-three-fixture"}}
	file, err := os.OpenFile(filepath.Join(stage, "events.jsonl"), os.O_EXCL|os.O_CREATE|os.O_WRONLY, 0600)
	require(err)
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	log := json.NewEncoder(file)
	record := func(event string, evidence any) {
		at, err := harbor.Clock()
		require(err)
		require(log.Encode(map[string]any{"event": event, "monotonic_ns": at, "vm_id": id, "data": evidence}))
	}

	record("create_start", map[string]any{"configured_tasks": 3, "app_image": app, "warehouse_image": warehouse, "network_devices": 0})
	got, err := m.Create(ctx, req)
	if got != "" {
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer done()
			if err := m.Stop(cleanup, got); err != nil {
				t.Error(err)
				return
			}
			requireThreeAllocationsAbsent(t, cleanup, m, ids)
			record("removed", ids)
		})
	}
	require(err)
	if got != id {
		t.Fatal("Manager.Create changed the VM identity")
	}
	require(m.Validate(ctx, id, req, nil))

	health := func() json.RawMessage {
		var result core.CommandResult
		var err error
		for i := 0; i < 30; i++ {
			result, err = m.Exec(ctx, id, core.Command{Path: "/usr/local/bin/node", Args: []string{"-e", mainTaskHealth}, Timeout: 3 * time.Second})
			if err == nil && result.ExitCode == 0 && json.Valid([]byte(result.Stdout)) {
				return json.RawMessage(strings.TrimSpace(result.Stdout))
			}
			time.Sleep(150 * time.Millisecond)
		}
		t.Fatalf("main container health check: %v, exit %d, %s", err, result.ExitCode, result.Stderr)
		return nil
	}
	beforeHealth := health()
	before := readThreeNativeTasks(t, ctx, m, id, ids)
	beforeVMM, err := m.checkpointProcesses(ctx, id)
	require(err)
	record("ready", map[string]any{"task_state": before, "http": beforeHealth, "host_processes": beforeVMM})

	record("snapshot_start", nil)
	require(m.Suspend(ctx, id))
	state, err := m.loadCheckpoint(id)
	require(err)
	require(confirmCheckpointProcesses(beforeVMM))
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
	var captured []string
	for _, task := range saved.Tasks {
		captured = append(captured, task.ID)
	}
	sort.Strings(captured)
	expected := append([]string{}, ids...)
	sort.Strings(expected)
	if !reflect.DeepEqual(captured, expected) || len(saved.Processes) != 3 || len(saved.Networks) != 0 {
		t.Fatalf("native checkpoint did not retain only the three local tasks: %v", captured)
	}
	capturedPIDs := map[string]uint32{}
	for _, process := range saved.Processes {
		if process.ExecID != "" || process.State.Pid == 0 {
			t.Fatalf("unexpected saved process %+v", process)
		}
		capturedPIDs[process.TaskID] = process.State.Pid
	}
	for _, task := range before {
		if capturedPIDs[task.ID] != task.PID {
			t.Fatalf("captured task PID differs: %+v", task)
		}
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(manifest))
	record("snapshot_complete", map[string]any{"manifest_sha256": digest, "task_ids": captured, "init_pids": capturedPIDs, "network_devices": len(saved.Networks), "original_vmm_absent": true})

	record("restore_start", nil)
	require(m.Resume(ctx, id))
	after := readThreeNativeTasks(t, ctx, m, id, ids)
	afterVMM, err := m.checkpointProcesses(ctx, id)
	require(err)
	afterHealth := health()
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeHealth, afterHealth) || beforeVMM[0] == afterVMM[0] {
		t.Fatalf("restore changed native task or health identity: before=%+v after=%+v", before, after)
	}
	retained, err := os.ReadFile(filepath.Join(state.NativeDirectory, "manifest.json"))
	require(err)
	if fmt.Sprintf("%x", sha256.Sum256(retained)) != digest {
		t.Fatal("native manifest changed during restore")
	}
	record("restore_complete", map[string]any{"task_state": after, "http": afterHealth, "host_processes": afterVMM, "manifest_sha256": digest})
	require(m.Stop(ctx, id))
	requireThreeAllocationsAbsent(t, ctx, m, ids)
	record("success_allocations_removed", ids)

	options.Companions[1].Command = []string{"/__aries_fixture_missing_command"}
	failing, err := New(options)
	require(err)
	t.Cleanup(func() {
		if err := failing.Close(); err != nil {
			t.Error(err)
		}
	})
	partialID := id + "-partial"
	req.Name = partialID
	req.Labels["aries.run"] = partialID
	partialIDs := []string{partialID, partialID + "-warehouse", partialID + "-health"}
	failed, failure := failing.Create(ctx, req)
	if failed != "" {
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer done()
			if err := failing.Stop(cleanup, failed); err != nil {
				t.Error(err)
			}
		})
	}
	if failed != partialID || failure == nil || !strings.Contains(failure.Error(), partialID+"-health") ||
		!strings.Contains(failure.Error(), "/__aries_fixture_missing_command") {
		t.Fatalf("partial fixture did not fail on the configured second companion: %q %v", failed, failure)
	}
	partial, err := failing.loadCheckpoint(partialID)
	require(err)
	if partial.Phase != "starting" || !reflect.DeepEqual(partial.CompanionIDs, partialIDs[1:]) {
		t.Fatalf("partial creation did not persist the complete owned task set: %+v", partial)
	}
	record("expected_partial_failure", map[string]any{"task_ids": partialIDs, "error": failure.Error()})
	require(failing.Stop(ctx, partialID))
	require(failing.Stop(ctx, partialID))
	requireThreeAllocationsAbsent(t, ctx, failing, partialIDs)
	record("partial_allocations_removed", partialIDs)

}

func requireThreeAllocationsAbsent(t *testing.T, ctx context.Context, m *Manager, ids []string) {
	t.Helper()
	for _, id := range ids {
		if _, err := m.client.LoadContainer(ctx, id); !errdefs.IsNotFound(err) {
			t.Fatalf("container %s remains after cleanup: %v", id, err)
		}
		if _, err := m.client.SnapshotService(m.options.Snapshotter).Stat(ctx, id); !errdefs.IsNotFound(err) {
			t.Fatalf("snapshot %s remains after cleanup: %v", id, err)
		}
	}
	if _, err := os.Stat(m.runtimeDir(ids[0])); !os.IsNotExist(err) {
		t.Fatalf("checkpoint state remains after cleanup: %v", err)
	}
}

func readThreeNativeTasks(t *testing.T, ctx context.Context, m *Manager, id string, ids []string) []nativeHealthTask {
	t.Helper()
	addr, err := os.ReadFile(filepath.Join(m.options.ShimDir, m.options.Namespace+"#"+id, "address"))
	if err != nil {
		t.Fatal(err)
	}
	path, closePath, err := shortUnixSocketPath(strings.TrimPrefix(strings.TrimSpace(string(addr)), "unix://"))
	if err != nil {
		t.Fatal(err)
	}
	defer closePath()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	rpc := ttrpc.NewClient(conn)
	defer rpc.Close()
	client := taskv3.NewTTRPCTaskClient(rpc)
	rows := make([]nativeHealthTask, 0, 3)
	for _, taskID := range ids {
		state, err := client.State(ctx, &taskv3.StateRequest{ID: taskID})
		if err != nil {
			t.Fatalf("native State %s: %v", taskID, err)
		}
		if state.ID != taskID || state.Pid == 0 || state.Status.String() != "RUNNING" {
			t.Fatalf("unexpected task state %s", state)
		}
		rows = append(rows, nativeHealthTask{state.ID, state.Pid, state.Status.String()})
	}
	return rows
}
