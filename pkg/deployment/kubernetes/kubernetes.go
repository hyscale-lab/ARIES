// Package kubernetes implements shared runtime deployment on Kubernetes pods.
//
// It drives the cluster through the kubectl binary rather than a client-go
// dependency: the kubeconfig and context kubectl resolves, or the pod's
// service account when it runs in-cluster, are the cluster contract.
//
// A pod cannot be created without being started, yet harnesses and the bridge
// stage private files between Create and Start. Every runtime therefore starts
// behind a gate: its container waits on a marker file that Start writes, and
// only then execs the requested command. The gate needs /bin/sh in the image.
package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

const (
	defaultNamespace      = "aries"
	defaultKubectl        = "kubectl"
	defaultReadyTimeout   = 5 * time.Minute
	defaultCleanupTimeout = 60 * time.Second
	containerName         = "runtime"
	gateDirectory         = "/run/aries-gate"
	gateMarker            = gateDirectory + "/start"
	// gateShell holds the requested command until Start writes the marker. The
	// fallback covers a sleep without fractional seconds.
	gateShell = `while [ ! -e ` + gateMarker + ` ]; do sleep 0.2 2>/dev/null || sleep 1; done; exec "$@"`
)

// commandRunner runs one kubectl invocation to completion.
type commandRunner interface {
	Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error
}

// Options configures one kubectl-backed transport.
type Options struct {
	// Namespace holds every pod, NetworkPolicy and Service this transport owns.
	Namespace string
	// NodeSelector pins pods to nodes carrying every listed label. Empty leaves
	// pods unpinned.
	NodeSelector map[string]string
	// PodCIDR and ServiceCIDR are excluded from the egress a network-allowed
	// task receives, keeping it off the cluster's own networks.
	PodCIDR, ServiceCIDR string
	// ServiceAccount is mounted into created pods. Empty mounts no token, which
	// is what every runtime except the bridge needs.
	ServiceAccount string
	// ImagePullSecret names a registry credential in Namespace.
	ImagePullSecret string
	// Images maps a locally built image, by full reference or by repository,
	// to what the cluster pulls.
	Images map[string]string
	// StagingDirs are made writable for StagingOwner before the container
	// starts, so a non-root image can receive its private archive there.
	StagingDirs  []string
	StagingOwner string

	KubectlPath    string
	ReadyTimeout   time.Duration
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// Manager implements deployment operations against one namespace.
type Manager struct {
	options Options
	kubectl commandRunner
	logger  *logrus.Logger
	// inCluster selects pod addresses over port-forwards for Address.
	inCluster bool
	forward   func(ctx context.Context, pod string, port int) (*portForward, error)

	mu       sync.Mutex
	forwards map[string]*portForward
}

var (
	_ deployment.Deployment     = (*Manager)(nil)
	_ deployment.ServiceRuntime = (*Manager)(nil)
)

// New configures a transport without contacting the cluster.
func New(options Options) (*Manager, error) {
	if options.Namespace == "" {
		options.Namespace = defaultNamespace
	}
	if !validName(options.Namespace) {
		return nil, fmt.Errorf("Kubernetes namespace %q is not a DNS label", options.Namespace)
	}
	if err := validateNodeSelector(options.NodeSelector); err != nil {
		return nil, err
	}
	for _, name := range []string{options.ServiceAccount, options.ImagePullSecret} {
		if name != "" && !validSubdomain(name) {
			return nil, fmt.Errorf("Kubernetes object name %q is invalid", name)
		}
	}
	for _, directory := range options.StagingDirs {
		if _, err := cleanContainerPath(directory); err != nil {
			return nil, fmt.Errorf("Kubernetes staging directory %q: %w", directory, err)
		}
	}
	if len(options.StagingDirs) != 0 {
		if err := validateNumericUser(options.StagingOwner); err != nil {
			return nil, fmt.Errorf("Kubernetes staging owner: %w", err)
		}
	}
	if options.KubectlPath == "" {
		options.KubectlPath = defaultKubectl
	}
	if options.ReadyTimeout <= 0 {
		options.ReadyTimeout = defaultReadyTimeout
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	logger := options.Logger
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	manager := &Manager{
		options: options, kubectl: execKubectl{path: options.KubectlPath}, logger: logger,
		inCluster: os.Getenv("KUBERNETES_SERVICE_HOST") != "",
		forwards:  make(map[string]*portForward),
	}
	manager.forward = manager.startPortForward
	return manager, nil
}

// Namespace returns the namespace this transport owns runtimes in.
func (manager *Manager) Namespace() string { return manager.options.Namespace }

// Close stops every port-forward this transport still holds.
func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	forwards := manager.forwards
	manager.forwards = make(map[string]*portForward)
	manager.mu.Unlock()
	for _, forward := range forwards {
		forward.stop()
	}
	return nil
}

// Create allocates the pod and waits for its container to reach the gate. The
// returned identity is the pod name, which callers generate with a random nonce.
func (manager *Manager) Create(ctx context.Context, request deployment.Request) (string, error) {
	if err := manager.validateRequest(request); err != nil {
		return "", err
	}
	manifest, err := manager.podManifest(request)
	if err != nil {
		return "", err
	}
	if _, err := manager.run(ctx, manifest, "create", "-n", manager.options.Namespace, "-f", "-"); err != nil {
		return manager.recoverCreate(ctx, request, fmt.Errorf("create Kubernetes pod: %w", err))
	}
	readyCtx, cancel := context.WithTimeout(ctx, manager.options.ReadyTimeout)
	defer cancel()
	timeout := strconv.Itoa(int(manager.options.ReadyTimeout.Seconds())) + "s"
	if _, err := manager.run(readyCtx, nil, "wait", "-n", manager.options.Namespace, "--for=jsonpath={.status.phase}=Running", "pod/"+request.Name, "--timeout="+timeout); err != nil {
		return request.Name, fmt.Errorf("wait for Kubernetes pod %s to run: %w%s", request.Name, err, manager.waitingReason(ctx, request.Name))
	}
	return request.Name, nil
}

// recoverCreate resolves a failed create. A lost response can hide an allocated
// pod, so absence is confirmed before reporting that nothing needs cleanup.
func (manager *Manager) recoverCreate(ctx context.Context, request deployment.Request, cause error) (string, error) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.options.CleanupTimeout)
	defer cancel()
	pod, found, err := manager.pod(recoveryCtx, request.Name)
	if err != nil {
		return "", errors.Join(cause, fmt.Errorf("%w: %w", deployment.ErrAllocationUnconfirmed, err))
	}
	if !found {
		return "", cause
	}
	for key, value := range request.Labels {
		if pod.Metadata.Annotations[key] != value {
			return "", errors.Join(cause, fmt.Errorf("%w: ownership differs from request", deployment.ErrAllocationUnconfirmed))
		}
	}
	return request.Name, cause
}

// waitingReason adds the kubelet's own explanation, such as ImagePullBackOff.
func (manager *Manager) waitingReason(ctx context.Context, name string) string {
	reasonCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	pod, found, err := manager.pod(reasonCtx, name)
	if err != nil || !found {
		return ""
	}
	var reasons []string
	for _, status := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
		if waiting := status.State.Waiting; waiting != nil && waiting.Reason != "" {
			reasons = append(reasons, waiting.Reason)
		}
	}
	if len(reasons) == 0 {
		return " (phase " + pod.Status.Phase + ")"
	}
	return " (" + strings.Join(reasons, ", ") + ")"
}

// Validate confirms identity, ownership and isolation, and that no supplied
// secret reached the pod object.
func (manager *Manager) Validate(ctx context.Context, id string, request deployment.Request, secrets [][]byte) error {
	if err := manager.validateRequest(request); err != nil {
		return err
	}
	raw, err := manager.run(ctx, nil, "get", "pod", "-n", manager.options.Namespace, id, "-o", "json")
	if err != nil {
		return fmt.Errorf("inspect Kubernetes pod: %w", err)
	}
	var pod podObject
	if err := json.Unmarshal(raw, &pod); err != nil {
		return fmt.Errorf("decode Kubernetes pod: %w", err)
	}
	if pod.Metadata.Name != id || request.Name != "" && id != request.Name || pod.Metadata.Namespace != manager.options.Namespace {
		return errors.New("deployment name differs from request")
	}
	for key, value := range request.Labels {
		if pod.Metadata.Annotations[key] != value {
			return errors.New("deployment labels do not match the task")
		}
	}
	if len(pod.Spec.Containers) != 1 {
		return errors.New("deployment must run exactly one container")
	}
	container := pod.Spec.Containers[0]
	expected, err := gatedCommand(request)
	if err != nil {
		return err
	}
	if container.Image != manager.image(request.Image) || !equalStrings(container.Command, expected) || len(container.Args) != 0 {
		return errors.New("deployment image or command differs from requested configuration")
	}
	if request.Workdir != "" && container.WorkingDir != request.Workdir {
		return errors.New("deployment workdir differs from request")
	}
	if request.NoNewPrivileges && (container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation) {
		return errors.New("deployment no-new-privileges is not enabled")
	}
	if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		return errors.New("deployment must not be privileged")
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		return errors.New("deployment must not share host namespaces")
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil {
			return errors.New("deployment has a host mount beyond requested paths")
		}
	}
	if manager.options.ServiceAccount == "" && (pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken) {
		return errors.New("deployment must not mount a service account token")
	}
	for _, secret := range secrets {
		if len(secret) > 0 && bytes.Contains(raw, secret) {
			return errors.New("deployment secret entered Kubernetes metadata")
		}
	}
	return nil
}

// Start releases the gate. Repeating it rewrites the same marker.
func (manager *Manager) Start(ctx context.Context, id string) error {
	if _, err := manager.run(ctx, nil, "exec", "-n", manager.options.Namespace, id, "-c", containerName, "--", "/bin/sh", "-c", ": > "+gateMarker); err != nil {
		return fmt.Errorf("release Kubernetes pod gate: %w", err)
	}
	return nil
}

// Running reports whether the pod's container is live. A pod waiting at its
// gate is running; application readiness belongs to the component.
func (manager *Manager) Running(ctx context.Context, id string) (bool, error) {
	pod, found, err := manager.pod(ctx, id)
	if err != nil || !found {
		return false, err
	}
	return pod.Status.Phase == "Running" && pod.Metadata.DeletionTimestamp == "", nil
}

// Logs returns bounded pod output.
func (manager *Manager) Logs(ctx context.Context, id string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("deployment log bound must be positive")
	}
	output := &boundedBuffer{limit: limit}
	if err := manager.LogsStream(ctx, id, output, output); err != nil {
		return nil, err
	}
	if output.exceeded {
		return nil, errors.New("deployment logs exceeded their bound")
	}
	return output.Bytes(), nil
}

// LogsStream copies pod output. The kubelet stores one interleaved stream, so
// everything is written to stdout.
func (manager *Manager) LogsStream(ctx context.Context, id string, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	var failure bytes.Buffer
	err := manager.kubectl.Run(ctx, nil, stdout, &failure, "logs", "-n", manager.options.Namespace, id, "-c", containerName)
	if err == nil {
		return nil
	}
	err = fmt.Errorf("read Kubernetes pod logs: %w: %s", err, strings.TrimSpace(failure.String()))
	if strings.Contains(failure.String(), "NotFound") {
		return fmt.Errorf("deployment logs: %w: %w", runner.ErrNotFound, err)
	}
	return err
}

// Address resolves an address the Runner can reach. In-cluster that is the pod
// address; elsewhere it is a loopback port-forward held until Stop or Close.
func (manager *Manager) Address(ctx context.Context, id string, port int) (string, error) {
	if port <= 0 || port > 65535 {
		return "", errors.New("deployment service port is invalid")
	}
	if manager.inCluster {
		return manager.TaskAddress(ctx, id, port)
	}
	key := id + ":" + strconv.Itoa(port)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if existing := manager.forwards[key]; existing != nil {
		return existing.address, nil
	}
	forward, err := manager.forward(ctx, id, port)
	if err != nil {
		return "", err
	}
	manager.forwards[key] = forward
	return forward.address, nil
}

// TaskAddress resolves the pod address other pods in the namespace dial.
func (manager *Manager) TaskAddress(ctx context.Context, id string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", errors.New("invalid task service port")
	}
	pod, found, err := manager.pod(ctx, id)
	if err != nil {
		return "", err
	}
	if !found || pod.Status.Phase != "Running" || net.ParseIP(pod.Status.PodIP) == nil {
		return "", errors.New("runtime task attachment is not ready")
	}
	return net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(port)), nil
}

// Stop deletes the pod and confirms that it is gone.
func (manager *Manager) Stop(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	manager.mu.Lock()
	for key, forward := range manager.forwards {
		if strings.HasPrefix(key, id+":") {
			forward.stop()
			delete(manager.forwards, key)
		}
	}
	manager.mu.Unlock()
	if _, err := manager.run(ctx, nil, "delete", "pod", "-n", manager.options.Namespace, id, "--ignore-not-found", "--grace-period=5", "--wait=true"); err != nil {
		return fmt.Errorf("delete Kubernetes pod: %w", err)
	}
	_, found, err := manager.pod(ctx, id)
	if err != nil {
		return fmt.Errorf("verify Kubernetes pod removal: %w", err)
	}
	if found {
		return errors.New("deployment remains after removal")
	}
	return nil
}

// pod reads one pod, reporting absence without an error.
func (manager *Manager) pod(ctx context.Context, id string) (podObject, bool, error) {
	raw, err := manager.run(ctx, nil, "get", "pod", "-n", manager.options.Namespace, id, "--ignore-not-found", "-o", "json")
	if err != nil {
		return podObject{}, false, fmt.Errorf("inspect Kubernetes pod: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return podObject{}, false, nil
	}
	var pod podObject
	if err := json.Unmarshal(raw, &pod); err != nil {
		return podObject{}, false, fmt.Errorf("decode Kubernetes pod: %w", err)
	}
	return pod, pod.Metadata.Name == id, nil
}

// run executes kubectl and returns its stdout. Failures carry kubectl's stderr.
func (manager *Manager) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	var input io.Reader
	if stdin != nil {
		input = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	if err := manager.kubectl.Run(ctx, input, &stdout, &stderr, args...); err != nil {
		return stdout.Bytes(), fmt.Errorf("kubectl %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// image returns the reference the cluster pulls for a requested image. A
// mapping keyed by the full reference wins; one keyed by the repository alone
// keeps the requested tag, which suits tags ARIES derives itself.
func (manager *Manager) image(requested string) string {
	if mapped := manager.options.Images[requested]; mapped != "" {
		return mapped
	}
	if separator := strings.LastIndex(requested, ":"); separator > strings.LastIndex(requested, "/") {
		if mapped := manager.options.Images[requested[:separator]]; mapped != "" {
			return mapped + requested[separator:]
		}
	}
	return requested
}

type execKubectl struct{ path string }

func (k execKubectl) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	command := exec.CommandContext(ctx, k.path, args...)
	command.Stdout, command.Stderr = stdout, stderr
	// Bound the wait for output pipes after cancellation kills kubectl.
	command.WaitDelay = 5 * time.Second
	if stdin != nil {
		// Copy input ourselves: handing the reader to os/exec would make Wait
		// block on a caller stream that stays open after the command exits.
		pipe, err := command.StdinPipe()
		if err != nil {
			return err
		}
		go func() {
			_, _ = io.Copy(pipe, stdin)
			_ = pipe.Close()
		}()
	}
	err := command.Run()
	if ctxErr := ctx.Err(); err != nil && ctxErr != nil {
		return ctxErr
	}
	return err
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *boundedBuffer) Write(content []byte) (int, error) {
	consumed := len(content)
	if remaining := buffer.limit - buffer.Len(); len(content) > remaining {
		content = content[:max(0, remaining)]
		buffer.exceeded = true
	}
	_, err := buffer.Buffer.Write(content)
	return consumed, err
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
