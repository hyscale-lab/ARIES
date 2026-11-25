package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
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

func TestCodexGroupChildLifetimes(t *testing.T) {
	if output, err := exec.Command("python3", "-B", "test_codex_group.py").CombinedOutput(); err != nil {
		t.Fatalf("process-group regression: %s: %v", output, err)
	}
}

// An optional integration control runs on digest-pinned workload images, over
// the same restored native guest transport and actual start/poll/release hooks.
func TestRestoredCodexGroupLifetimes(t *testing.T) {
	unit, dir := os.Getenv("ARIES_GROUP_TEST_UNIT"), os.Getenv("ARIES_GROUP_TEST_RUNTIME")
	if unit == "" || dir == "" {
		t.Skip("requires a private Firecracker runtime")
	}
	for _, image := range []string{
		"docker.io/alexgshaw/kv-store-grpc@sha256:3399400800dcb207634daa42bc1b052e831e285cc9d221eea66c47bc0fc79791",
		"docker.io/alexgshaw/hf-model-inference@sha256:0ef475aebda8abb8cbf3a1d97aee8ac4b09ca9720550e25a4a46a47930cd3bad",
	} {
		t.Run(strings.Split(filepath.Base(image), "@")[0], func(t *testing.T) {
			runGroupControl(t, unit, dir, image)
		})
	}
}

func runGroupControl(t *testing.T, unit, dir, image string) {
	ctx := t.Context()
	m, err := vhive.New(vhive.Options{IdleMode: "checkpoint", GuestTaskV3: true, Socket: filepath.Join("/run", unit, "containerd.sock"), Namespace: "aries", CheckpointDir: filepath.Join(dir, "checkpoints"), ShimDir: filepath.Join(dir, "shim"), IODir: filepath.Join("/run", unit, "aries-io"), VCPUs: 1, MemoryMB: 2048})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	req := deployment.Request{Name: fmt.Sprintf("aries-shell-group-test-%d", time.Now().UnixNano()), Image: image, StorageMB: 10240, Workdir: "/app", Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.component": "sandbox", "aries.kind": "task-container", "aries.run": "shell-group-control", "aries.task": "lifetime-control"}}
	id, err := m.Create(ctx, req)
	if id != "" {
		defer func() {
			if err := m.Stop(context.Background(), id); err != nil {
				t.Error(err)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(ctx, id, req, nil); err != nil {
		t.Fatal(err)
	}
	guest := func(ctx context.Context, command core.Command) (core.CommandResult, error) {
		return m.Exec(ctx, id, command)
	}
	if _, err := installCodexGroups(ctx, guest); err != nil {
		t.Fatal(err)
	}
	if err = m.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = m.Suspend(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = m.Resume(ctx, id); err != nil {
		t.Fatal(err)
	}
	check := func(script string) string {
		t.Helper()
		out, err := guest(ctx, core.Command{Path: "/bin/bash", Args: []string{"-c", script}})
		if err != nil || out.ExitCode != 0 {
			t.Fatalf("guest check: %+v %v", out, err)
		}
		return strings.TrimSpace(out.Stdout)
	}
	source := `set -e
sleep 60 </dev/null >/dev/null 2>&1 & ordinary=$!
nohup sleep 60 </dev/null >/dev/null 2>&1 & ignored_hup=$!
setsid /bin/bash -c 'touch detached-ready; exec sleep 60' </dev/null >/dev/null 2>&1 & detached=$!
while [ ! -f detached-ready ]; do sleep 0.01; done
printf '%s %s %s\n' "$ordinary" "$ignored_hup" "$detached" > children
touch started
while [ ! -f finish ]; do sleep 0.01; done
echo original-output
echo original-error >&2
exit 7`
	for _, useGroups := range []bool{false, true} {
		var g *shellGroups
		if useGroups {
			g, err = newShellGroups()
			if err != nil {
				t.Fatal(err)
			}
		}
		cwd := check("mktemp -d /tmp/aries-group-control.XXXXXX")
		sessions := harbor.NewProcessSessions(ctx, func(ctx context.Context, c replay.Call, stdout, stderr io.Writer) (core.CommandResult, error) {
			return m.ExecStream(ctx, id, g.start(sourceShellCommand("", "", c), c), nil, stdout, stderr)
		})
		defer sessions.Close()
		var execute shellCall = sessions.Execute
		if useGroups {
			execute = g.releaseOnExit(execute, guest)
		}
		start := replay.Call{Operation: "StartProcess", SessionID: 12, Sequence: 1, Script: source, Workdir: cwd, Login: false, YieldTime: 100 * time.Millisecond}
		out, err := execute(ctx, start)
		if err != nil || out.ExitCode != harbor.ProcessRunning {
			t.Fatalf("start: %+v %v", out, err)
		}
		check("cd " + cwd + " && timeout 3 bash -c 'while [ ! -f started ]; do sleep 0.01; done'")
		poll := replay.Call{Operation: "PollProcess", SessionID: 12, Sequence: 2, YieldTime: 100 * time.Millisecond}
		out, err = execute(ctx, poll)
		if err != nil || out.ExitCode != harbor.ProcessRunning {
			t.Fatalf("running poll: %+v %v", out, err)
		}
		state := `cd ` + cwd + `; for p in $(cat children); do if kill -0 "$p" 2>/dev/null; then printf 1; else printf 0; fi; done`
		first := check(state)
		check("touch " + cwd + "/finish")
		poll.YieldTime = 3 * time.Second
		out, err = execute(ctx, poll)
		if err != nil || out.ExitCode != 7 || out.Stdout != "original-output\n" || out.Stderr != "original-error\n" {
			t.Fatalf("terminal poll: %+v %v", out, err)
		}
		after := check(state)
		want := "111"
		if useGroups {
			want = "001"
		}
		if first != "111" || after != want {
			t.Fatalf("profile=%v: running %s, terminal %s", useGroups, first, after)
		}
		marker, _ := json.Marshal(map[string]any{"image": image, "group_profile": useGroups, "start_exit": -1, "empty_poll_exit": -1, "terminal_exit": out.ExitCode, "children": "ordinary, nohup, setsid", "before": first, "after": after, "stdout": out.Stdout, "stderr": out.Stderr})
		t.Logf("CONTROL %s", marker)
	}
}
