//go:build integration

package bridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	bridgewiring "github.com/hyscale-lab/aries/internal/app/wiring/bridge"
	deploymentwiring "github.com/hyscale-lab/aries/internal/app/wiring/deployment"
	"github.com/hyscale-lab/aries/internal/testutil/dockerroute"
	managed "github.com/hyscale-lab/aries/pkg/bridge"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	docker "github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/hyscale-lab/aries/pkg/runner"
	tasksandbox "github.com/hyscale-lab/aries/pkg/sandbox"
	"github.com/moby/moby/client"
	"golang.org/x/crypto/ssh"
)

const managedFixtureImage = "docker.io/library/debian:12-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241"

func managedRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		next := filepath.Dir(dir)
		if next == dir {
			t.Fatal("repository root missing")
		}
		dir = next
	}
}
func managedBinary(t *testing.T, name, env string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		path = filepath.Join(managedRoot(t), "bin", name)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("build %s before integration: %v", name, err)
	}
	return path
}
func managedQuote(v string) string { return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'" }

type managedFixture struct {
	service        *managed.Service
	runtime        *docker.Manager
	sandboxes      *tasksandbox.Manager
	environment    *docker.RunEnvironment
	protocol, root string
	ctx            context.Context
}

func newManagedFixture(t *testing.T, protocol string) *managedFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	versions, err := config.LoadVersions(filepath.Join(managedRoot(t), "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = docker.PullImages(ctx, "", []string{managedFixtureImage}); err != nil {
		t.Fatal(err)
	}
	env := dockerroute.Environment(t, "managed-integration")
	sandboxRuntime, err := docker.New(docker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sandboxes, err := tasksandbox.New(tasksandbox.Options{Deployment: sandboxRuntime, NewEnvironment: env.NewTaskEnvironment, OutputDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sandboxes.Close(); err != nil {
			t.Error(err)
		}
	})
	runtime, err := docker.New(docker.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cc, err := bridgewiring.SSHClientConfig(protocol, managedBinary(t, "aries-ssh-client", "ARIES_SSH_CLIENT"))
	if err != nil {
		t.Fatal(err)
	}
	launch := bridgewiring.Launch(versions.Bridge.Image)
	launch.RuntimeBackend, launch.ResourceMetrics = "docker", "docker-stats"
	launch.Config.Backend = "docker"
	launch.Config.BackendEndpoint, launch.Request.Mounts = deploymentwiring.DockerExecutionAccess("")
	service, err := managed.NewService(managed.Options{Runtime: runtime, Launch: launch, RunID: "managed-integration", Placement: core.RuntimePlacement{AttachmentID: env.NetworkName()}, OutputDir: filepath.Join(root, "infrastructure", "bridge"), Client: cc, BridgeType: protocol, RetainRawLog: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = service.Stop(cleanup)
	})
	if err = service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return &managedFixture{service: service, runtime: runtime, sandboxes: sandboxes, environment: env, protocol: protocol, root: root, ctx: ctx}
}

type managedSession struct {
	sandbox  *tasksandbox.Sandbox
	session  runner.ToolBridge
	endpoint core.ToolEndpoint
	root, id string
}

func (f *managedFixture) newSession(t *testing.T, index string) *managedSession {
	t.Helper()
	live, err := f.sandboxes.Start(f.ctx, core.SandboxRequest{RunID: "managed-integration", TaskID: "repeated-task", Environment: core.Environment{Image: managedFixtureImage, Workdir: "/work", MemoryMB: 64, Services: core.TaskServices{SearchPort: 8123}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := f.sandboxes.Stop(ctx, live); err != nil {
			t.Error(err)
		}
	})
	root := filepath.Join(f.root, index)
	bridge, err := f.service.NewSession(root)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := bridge.Start(f.ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*tasksandbox.Sandbox)
	d, err := sandbox.ExportBridgeTarget()
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.Connectivity().Placement.AttachmentID != f.environment.NetworkName() {
		t.Fatal("sandbox did not borrow run attachment")
	}
	data, err := os.ReadFile(filepath.Join(root, "repeated-task", "bridge", "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct{ RuntimeID, SandboxID, TargetRuntimeID string }
	if json.Unmarshal(data, &record) != nil || record.RuntimeID != f.service.RuntimeID() || record.SandboxID != d.SandboxID || record.TargetRuntimeID != sandbox.ContainerID() {
		t.Fatalf("wrong session binding %s", data)
	}
	return &managedSession{sandbox: sandbox, session: bridge, endpoint: endpoint, root: root, id: d.SandboxID}
}
func (f *managedFixture) wire(script string) string {
	if f.protocol == "hermes-ssh" {
		return "bash -l -c " + managedQuote(script)
	}
	return managedQuote("/bin/sh") + " " + managedQuote("-c") + " " + managedQuote(script)
}
func connectManaged(t *testing.T, e core.ToolEndpoint) (*ssh.Client, string) {
	t.Helper()
	var key string
	c, err := ssh.Dial("tcp", e.Address, &ssh.ClientConfig{User: e.Username, HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
		key = string(ssh.MarshalAuthorizedKey(k))
		return nil
	}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, key
}
func runManagedCommand(c *ssh.Client, command string, input []byte) ([]byte, []byte, int, error) {
	session, err := c.NewSession()
	if err != nil {
		return nil, nil, -1, err
	}
	defer session.Close()
	var out, stderr bytes.Buffer
	session.Stdin = bytes.NewReader(input)
	session.Stdout = &out
	session.Stderr = &stderr
	err = session.Run(command)
	code := 0
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitStatus()
		err = nil
	}
	return out.Bytes(), stderr.Bytes(), code, err
}
func TestManagedBridgeRuntimeMatrix(t *testing.T) {
	for _, protocol := range []string{"hermes-ssh", "openclaw-ssh"} {
		t.Run(protocol, func(t *testing.T) {
			f := newManagedFixture(t, protocol)
			a, b := f.newSession(t, "a"), f.newSession(t, "b")
			ca, ka := connectManaged(t, a.endpoint)
			cb, kb := connectManaged(t, b.endpoint)
			if a.endpoint.Address == b.endpoint.Address || a.id == b.id || ka != kb {
				t.Fatal("sessions did not share signer with distinct identity/listener")
			}
			if a.sandbox.Connectivity().SearchURL == b.sandbox.Connectivity().SearchURL {
				t.Fatal("repeated task IDs collided in service resolution")
			}
			for _, pair := range []struct {
				s *managedSession
				c *ssh.Client
			}{{a, ca}, {b, cb}} {
				result, err := pair.s.sandbox.Exec(f.ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "setsid sleep 120 </dev/null >/dev/null 2>&1 & echo $! > /work/benchmark.pid"}})
				if err != nil || result.ExitCode != 0 {
					t.Fatal(result, err)
				}
				script := "setsid sleep 120 </dev/null >/dev/null 2>&1 & echo $! > /work/tool.pid; cat > /work/state; cat /work/state; printf stderr >&2; exit 7"
				payload := []byte("managed-" + pair.s.id)
				out, stderr, code, err := runManagedCommand(pair.c, f.wire(script), payload)
				if err != nil || code != 7 || !bytes.Equal(out, payload) || string(stderr) != "stderr" {
					t.Fatal(string(out), string(stderr), code, err)
				}
				binary := []byte{0, 1, 255, 'b', 'i', 'n'}
				out, _, code, err = runManagedCommand(pair.c, f.wire("cat > /work/binary; cat /work/binary"), binary)
				if err != nil || code != 0 || !bytes.Equal(out, binary) {
					t.Fatal(out, code, err)
				}
			}
			// B remains in an active tool call while A releases and independently evaluates.
			bDone := make(chan error, 1)
			go func() {
				out, _, code, err := runManagedCommand(cb, f.wire("touch /work/busy; sleep 2; printf peer-complete"), nil)
				if err == nil && (code != 0 || string(out) != "peer-complete") {
					err = fmt.Errorf("peer command changed: %q %d", out, code)
				}
				bDone <- err
			}()
			for {
				result, err := b.sandbox.Exec(f.ctx, core.Command{Path: "/bin/test", Args: []string{"-f", "/work/busy"}})
				if err != nil {
					t.Fatal(err)
				}
				if result.ExitCode == 0 {
					break
				}
				if f.ctx.Err() != nil {
					t.Fatal(f.ctx.Err())
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := a.session.Stop(f.ctx); err != nil {
				t.Fatal(err)
			}
			if conn, err := net.DialTimeout("tcp", a.endpoint.Address, 200*time.Millisecond); err == nil {
				conn.Close()
				t.Fatal("released listener remained open")
			}
			evaluation, err := a.sandbox.Exec(f.ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", `for file in /work/benchmark.pid /work/tool.pid; do kill -0 "$(cat "$file")" || exit; done; cat /work/state`}})
			if err != nil || evaluation.ExitCode != 0 || evaluation.Stdout != "managed-"+a.id {
				t.Fatal(evaluation, err)
			}
			// Fresh evaluation sandboxes borrow the same network while live peers remain.
			fresh, err := f.sandboxes.Start(f.ctx, core.SandboxRequest{RunID: "managed-integration", TaskID: "repeated-task", Environment: core.Environment{Image: managedFixtureImage, Workdir: "/work", Services: core.TaskServices{SearchPort: 8123}}})
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Connectivity().Placement != b.sandbox.Connectivity().Placement || fresh.Connectivity().SearchURL == b.sandbox.Connectivity().SearchURL {
				t.Fatal("fresh evaluation attachment/name collision")
			}
			if err := f.sandboxes.Stop(f.ctx, fresh); err != nil {
				t.Fatal(err)
			}
			if err := <-bDone; err != nil {
				t.Fatal(err)
			}
			if err := f.sandboxes.Stop(f.ctx, a.sandbox); err != nil {
				t.Fatal(err)
			}
			if err := b.session.Stop(f.ctx); err != nil {
				t.Fatal(err)
			}
			// Fully idle service admits a later occurrence without replacing the runtime.
			c := f.newSession(t, "c")
			cc, kc := connectManaged(t, c.endpoint)
			if kc != ka {
				t.Fatal("service host key changed")
			}
			out, _, code, err := runManagedCommand(cc, f.wire("printf later"), nil)
			if err != nil || code != 0 || string(out) != "later" {
				t.Fatal(string(out), code, err)
			}
			if err := c.session.Stop(f.ctx); err != nil {
				t.Fatal(err)
			}
			for _, s := range []*managedSession{a, b, c} {
				for _, path := range s.endpoint.LogPaths {
					info, err := os.Stat(path)
					if err != nil || info.Mode().Perm() != 0600 {
						t.Fatal(path, err)
					}
				}
				for _, name := range []string{"id_ed25519", "known_hosts", "host.key", "authorized.pub"} {
					if _, err := os.Stat(filepath.Join(s.root, "repeated-task", "bridge", name)); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("credential staged", name, err)
					}
				}
				if s.endpoint.ClientSourceFile != "" {
					if _, err := os.Stat(s.endpoint.ClientSourceFile); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("helper retained", err)
					}
				}
			}
			api, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
			if err != nil {
				t.Fatal(err)
			}
			defer api.Close()
			inspection, err := api.ContainerInspect(f.ctx, f.service.RuntimeID(), client.ContainerInspectOptions{})
			if err != nil || !inspection.Container.State.Running || inspection.Container.Config.Labels["aries.task"] != "" {
				t.Fatal("shared bridge lost or task-owned", err)
			}
			if err := f.service.Stop(f.ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := api.ContainerInspect(f.ctx, f.service.RuntimeID(), client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
				t.Fatal("bridge removal unconfirmed", err)
			}
		})
	}
}
func TestManagedContainerCrashReportsMissingEvidence(t *testing.T) {
	f := newManagedFixture(t, "openclaw-ssh")
	s := f.newSession(t, "crash")
	if err := f.runtime.Stop(f.ctx, f.service.RuntimeID()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.session.Stop(f.ctx); err == nil || !strings.Contains(err.Error(), "without finalized evidence") {
			t.Fatal("crash lost evidence error", err)
		}
	}
	if err := f.service.Stop(f.ctx); err == nil || !strings.Contains(err.Error(), "without finalized evidence") {
		t.Fatal("run lost evidence error", err)
	}
}
