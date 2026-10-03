package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

// AttachOptions name a task container that another ARIES process created and
// still owns. Every field comes from that owner, never from the harness.
type AttachOptions struct {
	ContainerID string
	Network     string
	Workdir     string
	RunID       string
	TaskID      string
}

// Attacher hands the tool bridge handles on existing task containers when it
// runs apart from the runner.
type Attacher struct {
	client dockerClient
	logger *logrus.Logger
}

// NewAttacher connects to the Docker Engine socket, or the default.
func NewAttacher(socket string, logger *logrus.Logger) (*Attacher, error) {
	if socket == "" {
		socket = defaultDockerSocket
	}
	host := socket
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-bridge/1"))
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	return &Attacher{client: api, logger: logger}, nil
}

// Close releases the Docker client.
func (a *Attacher) Close() error {
	if closer, ok := a.client.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// Attach returns a handle that can exec into an existing task container but
// cannot stop it: it has no owning Manager, and Manager.Stop refuses a
// sandbox it does not own. The runner keeps the container's lifecycle.
//
// The container is checked before anything runs in it. The request names the
// container, so a wrong or malicious request could otherwise point the bridge
// at a harness container or another task's sandbox; Attach accepts only a
// running ARIES task container of that run and task, on its own task network.
func (a *Attacher) Attach(ctx context.Context, options AttachOptions) (*Sandbox, error) {
	if err := validateIdentity("run", options.RunID); err != nil {
		return nil, err
	}
	if err := validateIdentity("task", options.TaskID); err != nil {
		return nil, err
	}
	if options.ContainerID == "" || options.Network == "" {
		return nil, errors.New("attach needs a container ID and a network")
	}
	inspected, err := a.client.ContainerInspect(ctx, options.ContainerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect docker task container: %w", err)
	}
	c := inspected.Container
	if c.ID != options.ContainerID {
		return nil, fmt.Errorf("refusing to attach: %q is not the full ID of that container", options.ContainerID)
	}
	if c.Config == nil || c.Config.Labels["aries.component"] != "sandbox" || c.Config.Labels["aries.kind"] != "task-container" {
		return nil, fmt.Errorf("refusing to attach to container %s: it is not an ARIES task container", shortContainerID(c.ID))
	}
	// Start names the container and its network from one generated ID.
	// Requiring the pair means a request cannot pair a real task container
	// with some other network.
	suffix, ok := strings.CutPrefix(strings.TrimPrefix(c.Name, "/"), "aries-task-")
	if !ok || options.Network != "aries-net-"+suffix {
		return nil, fmt.Errorf("refusing to attach to container %s: network %q is not its task network", shortContainerID(c.ID), options.Network)
	}
	sandbox := &Sandbox{
		client: a.client, containerID: c.ID, containerName: strings.TrimPrefix(c.Name, "/"),
		networkName: options.Network, workdir: options.Workdir,
		runID: options.RunID, taskID: options.TaskID, cleanupTimeout: defaultCleanupTimeout,
	}
	// The same check Start makes on a fresh container: running, this run and
	// task, this workdir, attached to and labelled for this network.
	if err := sandbox.verifyLive(ctx); err != nil {
		return nil, fmt.Errorf("refusing to attach to container %s: %w", shortContainerID(c.ID), err)
	}
	return sandbox, nil
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
