package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

const defaultDockerSocket = "/var/run/docker.sock"

// dockerAPI is the small Engine surface the transport uses. The official
// client implements it; tests use a fake.
type dockerAPI interface {
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error)
	NetworkConnect(context.Context, string, client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	NetworkDisconnect(context.Context, string, client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error)
}

// DockerTransport reaches the aries-bridge container with docker exec, the
// Docker counterpart of KubeTransport. Docker has no flat pod network: each
// task has its own network, so the runner, which owns those networks,
// attaches the bridge to a task's network for the life of its grant. The
// grant listens only on that network's address, so a harness can reach its
// own grant and no other task's.
type DockerTransport struct {
	api dockerAPI
}

// NewDockerTransport connects to the Docker Engine socket, or the default.
func NewDockerTransport(socket string) (*DockerTransport, error) {
	if socket == "" {
		socket = defaultDockerSocket
	}
	if !strings.Contains(socket, "://") {
		socket = "unix://" + socket
	}
	api, err := client.New(client.WithHost(socket), client.WithUserAgent("aries-bridge-client/1"))
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &DockerTransport{api: api}, nil
}

// Close releases the Docker client.
func (t *DockerTransport) Close() error {
	if closer, ok := t.api.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// dockerSandbox is what the runner must know about a Docker sandbox to have
// the bridge container attach to it. The Docker sandbox satisfies it.
type dockerSandbox interface {
	runner.Sandbox
	ContainerID() string
	NetworkName() string
	Workdir() string
	RunID() string
	TaskID() string
}

var dockerContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Describe names a Docker sandbox container and its task network.
func (t *DockerTransport) Describe(generic runner.Sandbox) (SandboxRef, error) {
	sandbox, ok := generic.(dockerSandbox)
	if !ok || !dockerContainerID.MatchString(sandbox.ContainerID()) {
		return SandboxRef{}, errors.New("the bridge container can only serve a Docker sandbox")
	}
	return SandboxRef{
		Backend: BackendDocker, ContainerID: sandbox.ContainerID(), Network: sandbox.NetworkName(),
		Workdir: sandbox.Workdir(), RunID: sandbox.RunID(), TaskID: sandbox.TaskID(),
	}, nil
}

// Locate returns the one running bridge container. Its identity is its start
// time: a restarted container is a new daemon process holding no grants.
func (t *DockerTransport) Locate(ctx context.Context) (Target, error) {
	listed, err := t.api.ContainerList(ctx, client.ContainerListOptions{
		Filters: client.Filters{}.Add("label", ContainerLabel),
	})
	if err != nil {
		return Target{}, fmt.Errorf("find the bridge container: %w", err)
	}
	if len(listed.Items) != 1 {
		return Target{}, fmt.Errorf("need exactly one running bridge container (label %s), found %d; is `docker compose -f docker/docker-compose.yml up -d aries-bridge` running?", ContainerLabel, len(listed.Items))
	}
	inspected, err := t.api.ContainerInspect(ctx, listed.Items[0].ID, client.ContainerInspectOptions{})
	if err != nil {
		return Target{}, fmt.Errorf("inspect the bridge container: %w", err)
	}
	state := inspected.Container.State
	if state == nil || !state.Running || state.StartedAt == "" {
		return Target{}, errors.New("the bridge container is not running")
	}
	return Target{Name: shortID(inspected.Container.ID), Identity: state.StartedAt}, nil
}

// Exec runs aries-bridge in the bridge container.
func (t *DockerTransport) Exec(ctx context.Context, target Target, stdin io.Reader, stdout io.Writer, args ...string) error {
	created, err := t.api.ExecCreate(ctx, target.Name, client.ExecCreateOptions{
		AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true,
		Cmd: append([]string{Executable}, args...),
	})
	if err != nil {
		return fmt.Errorf("create exec in bridge %s: %w", target.Name, err)
	}
	attached, err := t.api.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("attach exec in bridge %s: %w", target.Name, err)
	}
	defer attached.Close()
	stop := context.AfterFunc(ctx, attached.Close)
	defer stop()
	writeErr := make(chan error, 1)
	if stdin != nil {
		go func() {
			_, err := io.Copy(attached.Conn, stdin)
			writeErr <- errors.Join(err, attached.CloseWrite())
		}()
	} else {
		writeErr <- nil
	}
	if stdout == nil {
		stdout = io.Discard
	}
	var stderr bytes.Buffer
	_, copyErr := stdcopy.StdCopy(stdout, &truncatingWriter{writer: &stderr, remaining: 64 << 10}, attached.Reader)
	if err := errors.Join(copyErr, <-writeErr, ctx.Err()); err != nil {
		return fmt.Errorf("exec in bridge %s: %w", target.Name, err)
	}
	exit, err := t.exitCode(ctx, created.ID)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("aries-bridge %s exited %d: %s", args[0], exit, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// exitCode waits for Docker to record the exec's exit, which can trail the
// end of its output stream by a moment.
func (t *DockerTransport) exitCode(ctx context.Context, execID string) (int, error) {
	for attempt := 0; ; attempt++ {
		inspected, err := t.api.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return -1, fmt.Errorf("inspect bridge exec: %w", err)
		}
		if !inspected.Running {
			return inspected.ExitCode, nil
		}
		if attempt == 100 {
			return -1, errors.New("bridge exec still running after its output ended")
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Gone reports whether the bridge container no longer exists, is not
// running, or was restarted since it was located.
func (t *DockerTransport) Gone(ctx context.Context, target Target) (bool, error) {
	inspected, err := t.api.ContainerInspect(ctx, target.Name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check bridge %s: %w", target.Name, err)
	}
	state := inspected.Container.State
	return state == nil || !state.Running || state.StartedAt != target.Identity, nil
}

// Join attaches the bridge to the task's network and returns its address
// there. Attaching twice is harmless, so a retried grant reuses it.
func (t *DockerTransport) Join(ctx context.Context, target Target, sandbox SandboxRef) (string, error) {
	if address, err := t.addressOn(ctx, target, sandbox.Network); err != nil || address != "" {
		return address, err
	}
	if _, err := t.api.NetworkConnect(ctx, sandbox.Network, client.NetworkConnectOptions{Container: target.Name}); err != nil {
		return "", fmt.Errorf("attach bridge %s to network %s: %w", target.Name, sandbox.Network, err)
	}
	address, err := t.addressOn(ctx, target, sandbox.Network)
	if err == nil && address == "" {
		err = fmt.Errorf("bridge %s has no IPv4 address on network %s", target.Name, sandbox.Network)
	}
	return address, err
}

// Leave detaches the bridge from the task's network, so the runner can
// remove it when the sandbox stops. A bridge that is gone, or no longer
// attached, has nothing to undo.
func (t *DockerTransport) Leave(ctx context.Context, target Target, sandbox SandboxRef) error {
	inspected, err := t.api.ContainerInspect(ctx, target.Name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect bridge %s: %w", target.Name, err)
	}
	if settings := inspected.Container.NetworkSettings; settings == nil || settings.Networks[sandbox.Network] == nil {
		return nil
	}
	_, err = t.api.NetworkDisconnect(ctx, sandbox.Network, client.NetworkDisconnectOptions{Container: target.Name})
	if err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("detach bridge %s from network %s: %w", target.Name, sandbox.Network, err)
	}
	return nil
}

// addressOn returns the bridge's IPv4 address on network, or "" when it is
// not attached.
func (t *DockerTransport) addressOn(ctx context.Context, target Target, network string) (string, error) {
	inspected, err := t.api.ContainerInspect(ctx, target.Name, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect bridge %s: %w", target.Name, err)
	}
	settings := inspected.Container.NetworkSettings
	if settings == nil || settings.Networks[network] == nil {
		return "", nil
	}
	if address := settings.Networks[network].IPAddress; address.Is4() {
		return address.String(), nil
	}
	return "", nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
