//go:build integration

package openclawssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/bridge/hermesssh"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const supervisionImage = "docker.io/library/debian:12-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241"

const supervisionLeaf = `#!/bin/bash
set -eu
IFS= read -r identity < /proc/$$/stat
printf '%s\n' "$identity" > "$1"
exec /bin/sleep 120
`

const supervisionMain = `#!/bin/bash
set -eu
IFS= read -r identity < /proc/$$/stat
printf '%s\n' "$identity" > /work/main.stat
setsid /bin/bash /work/supervision-leaf /work/setsid.stat </dev/null >/dev/null 2>&1 &
setsid /bin/bash -c '/bin/bash /work/supervision-leaf /work/doublefork.stat </dev/null >/dev/null 2>&1 &' </dev/null >/dev/null 2>&1 &
while [ ! -s /work/setsid.stat ] || [ ! -s /work/doublefork.stat ]; do sleep 0.02; done
if [ "$1" = canceled ]; then
  trap '' TERM
  while :; do sleep 1; done
fi
`

// Both adapters must retain background work across calls and retire the whole
// agent lineage before independent evaluation. The benchmark daemon is outside
// that lineage and must survive; PID plus start time avoids PID reuse ambiguity.
func TestBridgesRetainBackgroundWorkAndReapAllAgentDescendants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	supervisor := integrationSupervisorPath(t)
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if err := dockersandbox.PullImages(ctx, []string{supervisionImage}); err != nil {
		t.Fatal(err)
	}
	for _, harness := range []string{"openclaw", "hermes"} {
		for _, mode := range []string{"completed", "canceled"} {
			t.Run(harness+"/"+mode, func(t *testing.T) {
				outputDir := t.TempDir()
				logger := logrus.New()
				logger.SetOutput(io.Discard)
				sandboxes, err := dockersandbox.New(dockersandbox.Options{OutputDir: outputDir, Logger: logger})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = sandboxes.Close() })
				live, err := sandboxes.Start(ctx, core.SandboxRequest{
					RunID: "bridge-supervision-" + harness, TaskID: mode,
					Environment: core.Environment{Image: supervisionImage, Workdir: "/work", MemoryMB: 128},
				})
				if err != nil {
					t.Fatal(err)
				}
				sandbox := live.(*dockersandbox.Sandbox)
				t.Cleanup(func() {
					cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
					defer done()
					if err := sandboxes.Stop(cleanup, live); err != nil {
						t.Errorf("sandbox cleanup: %v", err)
					}
					if _, err := api.ContainerInspect(cleanup, sandbox.ContainerID(), client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
						t.Errorf("container remains: %v", err)
					}
					if _, err := api.NetworkInspect(cleanup, sandbox.NetworkName(), client.NetworkInspectOptions{}); !errdefs.IsNotFound(err) {
						t.Errorf("network remains: %v", err)
					}
				})
				for name, script := range map[string]string{"supervision-leaf": supervisionLeaf, "supervision-main": supervisionMain} {
					path := filepath.Join(outputDir, name)
					if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := sandbox.Upload(ctx, path, "/work/"+name); err != nil {
						t.Fatal(err)
					}
				}
				result, err := sandbox.Exec(ctx, core.Command{Path: "/bin/bash", Args: []string{"-c", "setsid /bin/bash /work/supervision-leaf /work/unrelated.stat </dev/null >/dev/null 2>&1 &"}})
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("start benchmark daemon: %+v, %v", result, err)
				}
				before := map[string]supervisionProcess{"unrelated": awaitSupervisionProcess(t, ctx, sandbox, "/work/unrelated.stat")}
				var bridge runner.ToolBridge
				switch harness {
				case "openclaw":
					bridge = newIntegrationBridge(t, outputDir, logger)
				case "hermes":
					bridge, err = hermesssh.New(hermesssh.Options{OutputDir: outputDir, SupervisorPath: supervisor, Logger: logger})
					if err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
					defer done()
					if err := bridge.Stop(cleanup); err != nil {
						t.Errorf("bridge cleanup: %v", err)
					}
				})
				endpoint, err := bridge.Start(ctx, sandbox)
				if err != nil {
					t.Fatal(err)
				}
				identity, err := os.ReadFile(endpoint.IdentitySourceFile)
				if err != nil {
					t.Fatal(err)
				}
				signer, err := ssh.ParsePrivateKey(identity)
				if err != nil {
					t.Fatal(err)
				}
				pinned, err := knownhosts.New(filepath.Join(filepath.Dir(endpoint.IdentitySourceFile), "known_hosts"))
				if err != nil {
					t.Fatal(err)
				}
				connection, err := ssh.Dial("tcp", endpoint.Address, &ssh.ClientConfig{
					User: endpoint.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
					HostKeyCallback: pinned, HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}, Timeout: 5 * time.Second,
				})
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close()
				stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
				defer stopCancellation()
				call, err := connection.NewSession()
				if err != nil {
					t.Fatal(err)
				}
				defer call.Close()
				call.Stdout, call.Stderr = io.Discard, io.Discard
				if err := call.Start(supervisionPayload(harness, "/bin/bash /work/supervision-main "+mode)); err != nil {
					t.Fatal(err)
				}
				finished := make(chan error, 1)
				go func() { finished <- call.Wait() }()
				for _, name := range []string{"main", "setsid", "doublefork"} {
					before[name] = awaitSupervisionProcess(t, ctx, sandbox, "/work/"+name+".stat")
				}
				if mode == "completed" {
					select {
					case err := <-finished:
						if err != nil {
							t.Fatalf("normal command failed: %v", err)
						}
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					for _, name := range []string{"setsid", "doublefork"} {
						process := before[name]
						probe, err := connection.NewSession()
						if err != nil {
							t.Fatal(err)
						}
						content, err := probe.Output(supervisionPayload(harness, fmt.Sprintf("/bin/cat /proc/%d/stat", process.pid)))
						_ = probe.Close()
						if err != nil {
							t.Fatalf("later tool call lost %s background process: %v", name, err)
						}
						after := parseSupervisionProcess(t, string(content))
						if after.pid != process.pid || after.startTime != process.startTime || after.state == "Z" || after.state == "X" {
							t.Fatalf("%s background process unavailable to later call: before=%+v after=%+v", name, process, after)
						}
					}
				}
				cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
				err = bridge.Stop(cleanup)
				done()
				if err != nil {
					t.Fatalf("Stop = %v", err)
				}
				if mode == "canceled" {
					select {
					case <-finished:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				for name, process := range before {
					after := readSupervisionProcess(t, ctx, sandbox, fmt.Sprintf("/proc/%d/stat", process.pid))
					same := after != nil && after.pid == process.pid && after.startTime == process.startTime
					if name == "unrelated" {
						if !same || after.state == "Z" || after.state == "X" {
							t.Errorf("benchmark daemon did not survive Stop: before=%+v after=%+v", process, after)
						}
					} else if same {
						t.Errorf("agent %s descendant survived Stop, including unreaped zombies: %+v", name, after)
					}
				}
				if _, err := os.Lstat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("identity remains after Stop: %v", err)
				}
			})
		}
	}
}

func supervisionPayload(harness, script string) string {
	if harness == "openclaw" {
		return encodeCanonicalTokens([]string{remoteShell, "-c", script})
	}
	return "bash -c '" + strings.ReplaceAll(script, "'", "'\"'\"'") + "'"
}

type supervisionProcess struct {
	pid       int
	startTime string
	state     string
}

func awaitSupervisionProcess(t *testing.T, ctx context.Context, sandbox *dockersandbox.Sandbox, path string) supervisionProcess {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if process := readSupervisionProcess(t, ctx, sandbox, path); process != nil {
			return *process
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process identity fixture was not ready: %s", path)
	return supervisionProcess{}
}

func readSupervisionProcess(t *testing.T, ctx context.Context, sandbox *dockersandbox.Sandbox, path string) *supervisionProcess {
	t.Helper()
	result, err := sandbox.Exec(ctx, core.Command{Path: "/bin/cat", Args: []string{path}})
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if result.ExitCode != 0 || strings.TrimSpace(result.Stdout) == "" {
		return nil
	}
	process := parseSupervisionProcess(t, result.Stdout)
	return &process
}

func parseSupervisionProcess(t *testing.T, content string) supervisionProcess {
	t.Helper()
	content = strings.TrimSpace(content)
	end, start := strings.LastIndex(content, ") "), strings.IndexByte(content, ' ')
	if start < 1 || end < start {
		t.Fatalf("invalid proc stat: %q", content)
	}
	fields := strings.Fields(content[end+2:])
	if len(fields) < 20 {
		t.Fatalf("short proc stat: %q", content)
	}
	pid, err := strconv.Atoi(content[:start])
	if err != nil {
		t.Fatal(err)
	}
	return supervisionProcess{pid: pid, startTime: fields[19], state: fields[0]}
}
