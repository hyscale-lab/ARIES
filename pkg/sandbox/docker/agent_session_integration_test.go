//go:build integration

package docker

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

func TestAgentSessionPreservesIdentityAndTrustedCleanup(t *testing.T) {
	for _, user := range []string{"0:0", "65532:65532"} {
		t.Run(user, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			sandbox := newAgentIntegrationSandbox(t, ctx, user)
			var output bytes.Buffer
			command := core.Command{Path: "/bin/sh", Args: []string{"-c", `set -eu
test ! -e /proc/self/fd/3
if /bin/cat /proc/$PPID/fd/3 >/dev/null 2>&1; then exit 41; fi
printf '%s:%s\n' "$(id -u)" "$(id -g)"
grep -Eq '^NoNewPrivs:[[:space:]]*1$' /proc/self/status
printf '%s\n' "$1" "$TASK_VALUE"
`, "identity-check", "literal ' argument\nnext line"}, Env: map[string]string{"TASK_VALUE": "binary-safe argv"}}
			result, err := sandbox.ExecAgentStream(ctx, command, nil, &output, io.Discard)
			if err != nil || result.ExitCode != 0 || output.String() != user+"\nliteral ' argument\nnext line\nbinary-safe argv\n" {
				t.Fatalf("identity/argv: result=%+v err=%v output=%q", result, err, output.String())
			}
			if user == "0:0" {
				// The second call must reexec the pinned inode even after a root
				// task replaces the staged pathname. Cleanup must never call rm.
				command = core.Command{Path: "/bin/sh", Args: []string{"-c", `set -eu
/bin/busybox rm "$1/supervisor"
printf '#!/bin/sh\nprintf invoked > /work/helper-invoked\nexit 1\n' > "$1/supervisor"
/bin/busybox chmod 755 "$1/supervisor"
/bin/busybox rm /bin/rm
printf '#!/bin/sh\nprintf invoked > /work/cleanup-invoked\nexit 0\n' > /bin/rm
/bin/busybox chmod 755 /bin/rm
`, "hostile-task", sandbox.agent.stage}}
				result, err = sandbox.ExecAgentStream(ctx, command, nil, io.Discard, io.Discard)
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("replace task-owned paths: %+v, %v", result, err)
				}
				result, err = sandbox.ExecAgentStream(ctx, core.Command{Path: "/bin/true"}, nil, io.Discard, io.Discard)
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("next call used replaced helper: %+v, %v", result, err)
				}
			}
			for range 2 {
				if err := sandbox.StopAgentSession(ctx); err != nil {
					t.Fatal(err)
				}
			}
			result, err = sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", `test ! -e "$1" && test ! -L "$1" && test ! -e /work/helper-invoked && test ! -e /work/cleanup-invoked`, "cleanup-check", sandbox.agent.stage}, User: rootExecUser})
			if err != nil || result.ExitCode != 0 {
				t.Fatalf("trusted cleanup/absence: %+v, %v", result, err)
			}
			if _, err := sandbox.ExecAgentStream(ctx, core.Command{Path: "/bin/true"}, nil, nil, nil); err == nil {
				t.Fatal("admitted a command after revocation")
			}
			if err := sandbox.StartAgentSession(ctx, os.Getenv("ARIES_EXEC_SUPERVISOR")); err == nil {
				t.Fatal("restarted a supervisor from task-modifiable paths")
			}
		})
	}
}

func TestAgentSessionKilledBrokerNeverConfirmsCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	sandbox := newAgentIntegrationSandbox(t, ctx, "0:0")
	result, err := sandbox.ExecAgentStream(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", `kill -KILL "$(awk '{print $4}' /proc/$PPID/stat)"; sleep 30`}}, nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatalf("killed broker returned confirmed result: %+v", result)
	}
	for range 2 {
		if err := sandbox.StopAgentSession(ctx); err == nil {
			t.Fatal("missing broker proof permitted evaluation")
		}
	}
}

func newAgentIntegrationSandbox(t *testing.T, ctx context.Context, user string) *Sandbox {
	t.Helper()
	supervisor := os.Getenv("ARIES_EXEC_SUPERVISOR")
	if supervisor == "" {
		t.Fatal("ARIES_EXEC_SUPERVISOR is required; run make integration")
	}
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	ensureFixtureImage(t, ctx, api)
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	manager, err := New(Options{OutputDir: t.TempDir(), Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	taskID := strings.NewReplacer("/", "-", ":", "-").Replace(t.Name())
	live, err := manager.Start(ctx, core.SandboxRequest{RunID: "agent-supervision", TaskID: taskID, Environment: core.Environment{Image: fixtureImage, Workdir: "/work", MemoryMB: 64, ExecUser: user}})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*Sandbox)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if err := manager.Stop(cleanup, live); err != nil {
			t.Errorf("sandbox cleanup: %v", err)
		}
		if _, err := api.ContainerInspect(cleanup, sandbox.ContainerID(), client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Errorf("container remains: %v", err)
		}
		if _, err := api.NetworkInspect(cleanup, sandbox.NetworkName(), client.NetworkInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Errorf("network remains: %v", err)
		}
	})
	if err := sandbox.StartAgentSession(ctx, supervisor); err != nil {
		t.Fatal(err)
	}
	return sandbox
}
