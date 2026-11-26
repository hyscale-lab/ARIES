package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

// This is a worker-only fixture check. It executes no recorded tool calls.
func TestRestoredWarehouseFixture(t *testing.T) {
	unit := os.Getenv("ARIES_NEXTJS_UNIT")
	if unit == "" {
		t.Skip("requires the two imported OCI fixtures and a private native runtime")
	}
	dir := os.Getenv("ARIES_NEXTJS_RUNTIME")
	if !filepath.IsAbs(dir) || filepath.Base(unit) != unit || os.Getenv("ARIES_NEXTJS_API_IMAGE") == "" {
		t.Fatal("invalid runtime or unit")
	}
	socket := filepath.Join("/run", unit)
	m, err := vhive.New(vhive.Options{
		IdleMode: "checkpoint", NativeCheckpoint: true, GuestTaskV3: true, Namespace: "aries",
		Socket: filepath.Join(socket, "containerd.sock"), IODir: filepath.Join(socket, "aries-io"),
		CheckpointDir: filepath.Join(dir, "checkpoints"), ShimDir: filepath.Join(dir, "shim"),
		VCPUs: 2, MemoryMB: 4096, CompanionImage: os.Getenv("ARIES_NEXTJS_API_IMAGE"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := t.Context()
	req := deployment.Request{Name: fmt.Sprintf("aries-warehouse-%d", time.Now().UnixNano()), Image: os.Getenv("ARIES_NEXTJS_APP_IMAGE"), StorageMB: 15360, Workdir: "/app", Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.component": "sandbox", "aries.kind": "task-container", "aries.run": "fixture-check", "aries.task": "nextjs-performance"}}
	id, err := m.Create(ctx, req)
	if id != "" {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := m.Stop(cleanup, id); err != nil {
				t.Error(err)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created %s", id)
	if err = m.Validate(ctx, id, req, nil); err != nil {
		t.Fatal(err)
	}
	guest := func(ctx context.Context, c core.Command) (core.CommandResult, error) { return m.Exec(ctx, id, c) }
	group, err := newShellGroups()
	if err != nil {
		t.Fatal(err)
	}
	group.python = "/opt/aries-python/bin/python3"
	if _, err = group.install(ctx, guest); err != nil {
		t.Fatal(err)
	}
	const probe = `(async()=>{for(const port of [4100,4101]){const r=await fetch("http://127.0.0.1:"+port+"/__health");if(!r.ok)throw Error(r.status)} const fs=require("fs");console.log(fs.readFileSync("/proc/sys/kernel/random/boot_id","utf8").trim(),fs.readFileSync("/tmp/warehouse-proof","utf8"),require("next/package.json").version)})().catch(e=>{console.error(e);process.exit(1)})`
	check := func(command core.Command) string {
		out, e := guest(ctx, command)
		if e != nil || out.ExitCode != 0 {
			t.Fatalf("fixture check: exit %d: %v %s", out.ExitCode, e, out.Stderr)
		}
		return strings.TrimSpace(out.Stdout)
	}
	check(core.Command{Path: "/bin/sh", Args: []string{"-ec", "cat /proc/sys/kernel/random/uuid > /tmp/warehouse-proof"}})
	cmd := core.Command{Path: "/usr/local/bin/node", Args: []string{"-e", probe}, Dir: "/app"}
	before := check(cmd)
	if err = m.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = m.Suspend(ctx, id); err != nil {
		t.Fatal(err)
	}
	t.Log("native checkpoint completed")
	if err = m.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	if after := check(cmd); before != after || !strings.HasSuffix(after, "15.4.10") {
		t.Fatalf("fixture identity changed: %q -> %q", before, after)
	}
	t.Log("restored both loopback health endpoints, guest boot ID and main task file")
	group.onlyInterruptible = true
	group.interruptible = map[int]bool{1: true}
	sessions := harbor.NewCodexProcessSessions(ctx, func(ctx context.Context, c replay.Call, stdout, stderr io.Writer) (core.CommandResult, error) {
		return m.ExecStream(ctx, id, group.start(sourceShellCommand("", "", c), c), nil, stdout, stderr)
	})
	defer sessions.Close()
	sessions.SendInterrupt = func(ctx context.Context, c replay.Call) error { return group.interrupt(ctx, guest, c) }
	execute := group.releaseOnExit(sessions.Execute, guest)
	start := replay.Call{Operation: "StartProcess", SessionID: 1, Sequence: 1, Script: "exec /bin/sleep 60", Workdir: "/app", YieldTime: time.Second}
	if out, err := execute(ctx, start); err != nil || out.ExitCode != harbor.ProcessRunning {
		t.Fatalf("start exit %d, err %v", out.ExitCode, err)
	}
	poll := replay.Call{Operation: "PollProcess", PipeInterrupt: true, SessionID: 1, Sequence: 2, YieldTime: time.Second}
	if out, err := execute(ctx, poll); err != nil || out.ExitCode != 130 {
		t.Fatalf("interrupt exit %d, err %v", out.ExitCode, err)
	}
	t.Log("restored native pipe handle returned running, then SIGINT exit 130")

}
