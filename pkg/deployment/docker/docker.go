// Package docker implements shared runtime deployment through the Moby SDK.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

type dockerClient interface {
	NetworkCreate(context.Context, string, client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkRemove(context.Context, string, client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
	ExecStart(context.Context, string, client.ExecStartOptions) (client.ExecStartResult, error)
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	CopyToContainer(context.Context, string, client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerTop(context.Context, string, client.ContainerTopOptions) (client.ContainerTopResult, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error)
	ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

// Options configures a per-harness Docker transport.
type Options struct {
	Socket         string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// Manager implements deployment operations; the harness owns its task lifecycle.
type Manager struct {
	client    dockerClient
	logger    *logrus.Logger
	closeOnce sync.Once
	closeErr  error
	timeout   time.Duration
}

var _ deployment.Deployment = (*Manager)(nil)

// New configures a transport without contacting the Docker daemon.
func New(options Options) (*Manager, error) {
	host := options.Socket
	if host == "" {
		host = "/var/run/docker.sock"
	}
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-deployment/1"))
	if err != nil {
		return nil, fmt.Errorf("create Docker deployment client: %w", err)
	}
	logger := options.Logger
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	return &Manager{client: api, logger: logger, timeout: options.CleanupTimeout}, nil
}

func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	manager.closeOnce.Do(func() {
		if closer, ok := manager.client.(interface{ Close() error }); ok {
			manager.closeErr = closer.Close()
		}
	})
	return manager.closeErr
}

func resources(request deployment.Request) (container.Resources, error) {
	var result container.Resources
	if request.CPU != nil {
		scaled := *request.CPU * 1e9
		if *request.CPU <= 0 || math.IsNaN(*request.CPU) || math.IsInf(*request.CPU, 0) || scaled >= math.Exp2(63) {
			return result, errors.New("deployment CPU must be finite, positive, and convert to NanoCPUs below 2^63")
		}
		result.NanoCPUs = int64(scaled)
	}
	if request.MemoryMB != nil {
		if *request.MemoryMB <= 0 || int64(*request.MemoryMB) > math.MaxInt64>>20 {
			return result, fmt.Errorf("deployment memory must be positive and no greater than %d MiB", int64(math.MaxInt64)>>20)
		}
		result.Memory = int64(*request.MemoryMB) << 20
	}
	if request.GPUs < 0 || request.StorageMB < 0 {
		return result, errors.New("deployment GPU and storage bounds must be nonnegative")
	}
	if request.GPUs > 0 {
		result.DeviceRequests = []container.DeviceRequest{{Driver: "nvidia", Count: request.GPUs, Capabilities: [][]string{{"gpu"}}}}
	}
	return result, nil
}

func (manager *Manager) Create(ctx context.Context, request deployment.Request) (string, error) {
	if err := validateRuntimeRequest(request); err != nil {
		return "", err
	}
	limits, err := resources(request)
	if err != nil {
		return "", err
	}
	if request.ServicePort < 0 || request.ServicePort > 65535 {
		return "", errors.New("deployment service port is invalid")
	}
	config := &container.Config{Image: request.Image, WorkingDir: request.Workdir, Env: request.Env, Entrypoint: request.Entrypoint, Cmd: request.Args, Labels: request.Labels}
	host := &container.HostConfig{NetworkMode: container.NetworkMode(request.Placement.AttachmentID), Resources: limits}
	for _, declared := range request.Mounts {
		host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeBind, Source: declared.Source, Target: declared.Target, ReadOnly: declared.ReadOnly})
	}
	if request.Init {
		host.Init = boolPointer(true)
	}
	if request.NoNewPrivileges {
		host.SecurityOpt = []string{"no-new-privileges=true"}
	}
	if request.StorageMB > 0 {
		host.StorageOpt = map[string]string{"size": fmt.Sprintf("%dm", request.StorageMB)}
	}
	var networking *network.NetworkingConfig
	if len(request.NetworkAliases) > 0 {
		networking = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{request.Placement.AttachmentID: {Aliases: request.NetworkAliases}}}
	}
	if request.ServicePort != 0 {
		port := network.MustParsePort(strconv.Itoa(request.ServicePort) + "/tcp")
		config.ExposedPorts = network.PortSet{port: struct{}{}}
		host.PortBindings = network.PortMap{port: []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1")}}}
	}
	if request.InternalPort != 0 {
		if config.ExposedPorts == nil {
			config.ExposedPorts = network.PortSet{}
		}
		config.ExposedPorts[network.MustParsePort(strconv.Itoa(request.InternalPort)+"/tcp")] = struct{}{}
	}
	created, err := manager.client.ContainerCreate(ctx, client.ContainerCreateOptions{Name: request.Name, Config: config, HostConfig: host, NetworkingConfig: networking})
	if strings.TrimSpace(created.ID) != "" {
		return created.ID, err
	}
	if err == nil {
		err = errors.New("Docker returned an empty deployment identity")
	}
	if request.Name == "" {
		return "", errors.Join(err, fmt.Errorf("%w: no requested name", deployment.ErrAllocationUnconfirmed))
	}
	// A canceled/lost create response can hide an allocated container. Recover an
	// immutable cleanup identity only after checking this occurrence's ownership.
	timeout := manager.timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	inspection, lookupErr := manager.client.ContainerInspect(recoveryCtx, request.Name, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(lookupErr) {
		return "", err
	}
	if lookupErr != nil {
		return "", errors.Join(err, fmt.Errorf("%w: %w", deployment.ErrAllocationUnconfirmed, lookupErr))
	}
	info := inspection.Container
	if info.ID == "" || info.Name != "/"+request.Name || info.Config == nil || len(request.Labels) == 0 {
		return "", errors.Join(err, fmt.Errorf("%w: identity or ownership could not be verified", deployment.ErrAllocationUnconfirmed))
	}
	for key, value := range request.Labels {
		if info.Config.Labels[key] != value {
			return "", errors.Join(err, fmt.Errorf("%w: ownership differs from request", deployment.ErrAllocationUnconfirmed))
		}
	}
	return info.ID, err
}

func (manager *Manager) Validate(ctx context.Context, id string, request deployment.Request, secrets [][]byte) error {
	if err := validateRuntimeRequest(request); err != nil {
		return err
	}
	inspection, err := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return err
	}
	info := inspection.Container
	if info.ID != id || info.Config == nil || info.HostConfig == nil {
		return errors.New("deployment inspection is incomplete")
	}
	config := info.Config
	if request.Name != "" && info.Name != "/"+request.Name {
		return errors.New("deployment name differs from request")
	}
	if request.Workdir != "" && config.WorkingDir != request.Workdir {
		return errors.New("deployment workdir differs from request")
	}
	if request.NoNewPrivileges && !noNewPrivilegesEnabled(info.HostConfig.SecurityOpt) {
		return errors.New("deployment no-new-privileges is not enabled")
	}
	if request.Init && (info.HostConfig.Init == nil || !*info.HostConfig.Init) {
		return errors.New("deployment init is not enabled")
	}
	if config.Image != request.Image || !slices.Equal(config.Cmd, request.Args) || request.Entrypoint != nil && !slices.Equal(config.Entrypoint, request.Entrypoint) {
		return errors.New("deployment image or command differs from requested configuration")
	}
	for key, value := range request.Labels {
		if config.Labels[key] != value {
			return errors.New("deployment labels do not match the task")
		}
	}
	metadata := append(append(append([]string{}, config.Env...), config.Cmd...), config.Entrypoint...)
	for _, value := range config.Labels {
		metadata = append(metadata, value)
	}
	for _, value := range metadata {
		for _, secret := range secrets {
			if len(secret) > 0 && bytes.Contains([]byte(value), secret) {
				return errors.New("deployment secret entered Docker metadata")
			}
		}
	}
	if len(request.NetworkAliases) > 0 && info.State != nil && info.State.Running {
		if info.NetworkSettings == nil || info.NetworkSettings.Networks[request.Placement.AttachmentID] == nil {
			return errors.New("deployment task network is not attached")
		}
	}
	if string(info.HostConfig.NetworkMode) != request.Placement.AttachmentID {
		return errors.New("deployment must use only the task network")
	}
	return validateMounts(info, request)
}

func validateMounts(info container.InspectResponse, request deployment.Request) error {
	if len(info.HostConfig.Binds) != 0 || len(info.HostConfig.Mounts) != len(request.Mounts) {
		return errors.New("deployment host mounts differ from request")
	}
	requested := make(map[string]deployment.Mount, len(request.Mounts))
	for _, declared := range request.Mounts {
		requested[declared.Target] = declared
	}
	configured := make(map[string]bool, len(requested))
	for _, actual := range info.HostConfig.Mounts {
		declared, ok := requested[actual.Target]
		if !ok || configured[actual.Target] || actual.Type != mount.TypeBind || actual.Source != declared.Source || actual.ReadOnly != declared.ReadOnly {
			return errors.New("deployment host mount differs from request")
		}
		configured[actual.Target] = true
	}
	present := make(map[string]bool, len(requested))
	for _, actual := range info.Mounts {
		if declared, ok := requested[actual.Destination]; ok {
			if present[actual.Destination] || actual.Type != mount.TypeBind || actual.Source != declared.Source || actual.RW == declared.ReadOnly {
				return errors.New("deployment mounted path differs from request")
			}
			present[actual.Destination] = true
			continue
		}
		if actual.Type != mount.TypeVolume || actual.Name == "" || (!request.AllowImageVolumes && !slices.Contains(request.ImageVolumes, actual.Destination)) {
			return errors.New("deployment has a mount beyond requested paths and image-declared volumes")
		}
	}
	if len(present) != len(requested) {
		return errors.New("deployment requested mount is missing")
	}
	return nil
}

func (manager *Manager) UploadArchive(ctx context.Context, id, destination string, archive io.Reader) error {
	_, err := manager.client.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: destination, Content: archive, CopyUIDGID: true})
	return err
}

func (manager *Manager) DownloadArchive(ctx context.Context, id, path string) (io.ReadCloser, deployment.FileInfo, error) {
	result, err := manager.client.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: path})
	if errdefs.IsNotFound(err) {
		if _, inspectErr := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{}); inspectErr == nil {
			return nil, deployment.FileInfo{}, fmt.Errorf("deployment archive: %w: %w", runner.ErrNotFound, err)
		}
	}
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	return result.Content, deployment.FileInfo{Size: result.Stat.Size, Mode: result.Stat.Mode}, nil
}

func (manager *Manager) Start(ctx context.Context, id string) error {
	_, err := manager.client.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (manager *Manager) Running(ctx context.Context, id string) (bool, error) {
	result, err := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return result.Container.State != nil && result.Container.State.Running, nil
}

func (manager *Manager) Logs(ctx context.Context, id string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("deployment log bound must be positive")
	}
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = limit, limit
	copyErr := manager.LogsStream(ctx, id, &stdout, &stderr)
	var boundErr error
	if stdout.exceeded || stderr.exceeded {
		boundErr = errors.New("deployment logs exceeded their bound")
	}
	if err := errors.Join(copyErr, boundErr); err != nil {
		return nil, err
	}
	return append(stdout.Bytes(), stderr.Bytes()...), nil
}

func (manager *Manager) Address(ctx context.Context, id string, port int) (string, error) {
	if port <= 0 || port > 65535 {
		return "", errors.New("deployment service port is invalid")
	}
	inspection, err := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	if inspection.Container.NetworkSettings == nil {
		return "", errors.New("deployment service port binding is missing")
	}
	servicePort := network.MustParsePort(strconv.Itoa(port) + "/tcp")
	bindings := inspection.Container.NetworkSettings.Ports[servicePort]
	if len(bindings) != 1 {
		return "", fmt.Errorf("deployment service requires exactly one host port binding, got %d", len(bindings))
	}
	binding := bindings[0]
	if binding.HostIP.String() != "127.0.0.1" {
		return "", errors.New("deployment service host port must bind 127.0.0.1")
	}
	value, err := strconv.ParseUint(binding.HostPort, 10, 16)
	if err != nil || value == 0 {
		return "", errors.New("deployment service host port was not published")
	}
	return net.JoinHostPort("127.0.0.1", binding.HostPort), nil
}

func (manager *Manager) Stop(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	var errs []error
	inspection, inspectErr := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(inspectErr) {
		return nil
	}
	if inspectErr != nil {
		errs = append(errs, fmt.Errorf("inspect deployment before stop: %w", inspectErr))
	}
	if inspectErr != nil || inspection.Container.State == nil || inspection.Container.State.Running {
		timeout := 5
		if _, err := manager.client.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("stop deployment: %w", err))
		}
		inspection, inspectErr = manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if inspectErr != nil && !errdefs.IsNotFound(inspectErr) {
			errs = append(errs, fmt.Errorf("inspect deployment after stop: %w", inspectErr))
		}
		if !errdefs.IsNotFound(inspectErr) && (inspectErr != nil || inspection.Container.State == nil || inspection.Container.State.Running) {
			if _, err := manager.client.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: "KILL"}); err != nil && !errdefs.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("kill deployment: %w", err))
			}
		}
	}
	if _, err := manager.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("remove deployment: %w", err))
	}
	if _, err := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{}); err == nil {
		return errors.Join(append(errs, errors.New("deployment remains after removal"))...)
	} else if !errdefs.IsNotFound(err) {
		return errors.Join(append(errs, fmt.Errorf("verify deployment removal: %w", err))...)
	}
	if warning := errors.Join(errs...); warning != nil {
		logger := manager.logger
		if logger == nil {
			logger = logrus.StandardLogger()
		}
		logger.WithContext(ctx).WithError(warning).Warn("deployment cleanup recovered after lifecycle errors")
	}
	return nil
}

func boolPointer(value bool) *bool { return &value }
func noNewPrivilegesEnabled(options []string) bool {
	return slices.ContainsFunc(options, func(option string) bool {
		return option == "no-new-privileges" || option == "no-new-privileges=true" || option == "no-new-privileges:true"
	})
}
func (manager *Manager) LogsStream(ctx context.Context, id string, stdout, stderr io.Writer) error {
	logs, err := manager.client.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if errdefs.IsNotFound(err) {
		return fmt.Errorf("deployment logs: %w: %w", runner.ErrNotFound, err)
	}
	if err != nil {
		return err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	_, copyErr := stdcopy.StdCopy(stdout, stderr, logs)
	return errors.Join(copyErr, logs.Close())
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	consumed := len(content)
	remaining := buffer.limit - buffer.Len()
	if len(content) > remaining {
		content = content[:max(0, remaining)]
		buffer.exceeded = true
	}
	_, err := buffer.Buffer.Write(content)
	return consumed, err
}
func (manager *Manager) CreateNetwork(ctx context.Context, request NetworkRequest) (string, error) {
	result, err := manager.client.NetworkCreate(ctx, request.Name, client.NetworkCreateOptions{Driver: "bridge", Internal: request.Internal, Labels: request.Labels})
	if err != nil {
		return result.ID, err
	}
	if result.ID == "" {
		return "", errors.New("Docker returned an empty network identity")
	}
	return result.ID, nil
}
func validateNetwork(result client.NetworkInspectResult, id string, request NetworkRequest) error {
	n := result.Network
	if n.ID != id || n.Name != request.Name || n.Internal != request.Internal || n.Driver != "bridge" {
		return errors.New("deployment network identity or isolation differs from request")
	}
	for k, v := range request.Labels {
		if n.Labels[k] != v {
			return errors.New("deployment network ownership labels differ from request")
		}
	}
	return nil
}
func (manager *Manager) ValidateNetwork(ctx context.Context, id string, request NetworkRequest) error {
	result, err := manager.client.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
	if err != nil {
		return err
	}
	return validateNetwork(result, id, request)
}
func (manager *Manager) StopNetwork(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	_, removeErr := manager.client.NetworkRemove(ctx, id, client.NetworkRemoveOptions{})
	_, inspectErr := manager.client.NetworkInspect(ctx, id, client.NetworkInspectOptions{})
	if errdefs.IsNotFound(inspectErr) {
		return nil
	}
	if inspectErr == nil {
		return errors.Join(removeErr, errors.New("deployment network remains after removal"))
	}
	return errors.Join(removeErr, fmt.Errorf("verify deployment network removal: %w", inspectErr))
}

func validatePlacement(placement core.RuntimePlacement) error {
	if strings.TrimSpace(placement.AttachmentID) == "" {
		return errors.New("Docker deployment requires a nonempty Docker task attachment")
	}
	return nil
}

func validateRuntimeRequest(request deployment.Request) error {
	if err := validatePlacement(request.Placement); err != nil {
		return err
	}
	if request.InternalPort < 0 || request.InternalPort > 65535 {
		return errors.New("invalid internal service port")
	}
	targets := make(map[string]bool, len(request.Mounts))
	for _, declared := range request.Mounts {
		if !filepath.IsAbs(declared.Source) || filepath.Clean(declared.Source) != declared.Source || !filepath.IsAbs(declared.Target) || filepath.Clean(declared.Target) != declared.Target || strings.ContainsRune(declared.Source+declared.Target, 0) {
			return errors.New("deployment mounts require absolute clean source and target paths")
		}
		if targets[declared.Target] {
			return errors.New("deployment mount targets must be distinct")
		}
		targets[declared.Target] = true
	}
	return nil
}

// TaskAddress resolves the immutable runtime on its sole task network.
func (manager *Manager) TaskAddress(ctx context.Context, id string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", errors.New("invalid task service port")
	}
	result, err := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return "", err
	}
	c := result.Container
	if c.ID != id || c.State == nil || !c.State.Running || c.NetworkSettings == nil || len(c.NetworkSettings.Networks) != 1 {
		return "", errors.New("runtime task attachment is not ready")
	}
	for _, endpoint := range c.NetworkSettings.Networks {
		if endpoint == nil || !endpoint.IPAddress.IsValid() {
			return "", errors.New("runtime task address is missing")
		}
		return net.JoinHostPort(endpoint.IPAddress.String(), strconv.Itoa(port)), nil
	}
	return "", errors.New("runtime task attachment is missing")
}
