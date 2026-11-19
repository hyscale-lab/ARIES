package apptainer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// NetworkPolicy names the connectivity of an Apptainer run: Runner and the
// bridge in one private network namespace with internet access and no route to
// the host's loopback, and each task's runtimes in a namespace of their own
// nested in it, with internet access and a route to the run's loopback.
const NetworkPolicy = "task-namespace"

// RunEnvironment is the run's network namespace, which RunInNamespace created
// before the run began. There is nothing to allocate or remove for the run: the
// namespace ends with the run's last process. Start confirms the run is inside
// it. Each task handle starts a namespace of its own.
type RunEnvironment struct {
	mu               sync.Mutex
	runID, network   string
	started, stopped bool
	current          func() (string, error)
	stateDir         string
	// harnessModel is the run's model base URL as a harness in a task
	// namespace reaches it, when that differs from the run's.
	harnessModel string
	// tasks counts the task namespaces started, which numbers their loopback
	// addresses.
	tasks          uint32
	startNamespace func(context.Context, taskNamespaceRequest) (taskNamespaceHandle, error)
}

// NewRunEnvironment returns the environment of a run whose model is at
// modelBaseURL.
func (manager *Manager) NewRunEnvironment(runID, modelBaseURL string) *RunEnvironment {
	return &RunEnvironment{
		runID: runID, current: currentRunNamespace, stateDir: manager.options.StateDir,
		harnessModel: harnessModelURL(modelBaseURL), startNamespace: startTaskNamespace,
	}
}

// harnessModelURL returns the model base URL a harness in a task namespace
// reaches when the run's is Runner's own 127.0.0.1, as a managed model
// server's is: the same URL with the task's gateway, which slirp4netns
// connects to the run namespace's 127.0.0.1. Any other URL needs no change, or
// CheckModelAddress refuses it, and nothing is returned for it.
func harnessModelURL(base string) string {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" || !gatewayLeadsTo(parsed.Hostname()) {
		return ""
	}
	port := parsed.Port()
	parsed.Host = taskGateway
	if port != "" {
		parsed.Host = net.JoinHostPort(taskGateway, port)
	}
	return parsed.String()
}

// gatewayLeadsTo reports whether a host names the run namespace's 127.0.0.1,
// the one loopback address a task's gateway leads to: 127.0.0.1 itself,
// localhost, or the unspecified address, which connects to it.
func gatewayLeadsTo(host string) bool {
	ip := net.ParseIP(host)
	return isLocalhost(host) || ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv4zero))
}

// CheckManagedListener refuses the host a managed model server listens on
// when a task's gateway would not reach it: the gateway leads to the run
// namespace's 127.0.0.1, so the server must listen there or on every IPv4
// address. A server that Runner reaches on another loopback address, such as
// ::1 through localhost, would pass its health check and fail the harness.
func CheckManagedListener(host string) error {
	if ip := net.ParseIP(host); ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv4zero)) {
		return nil
	}
	return fmt.Errorf("an Apptainer run's tasks cannot reach a managed model server listening on %s: they reach the run's loopback only at 127.0.0.1, through their gateway %s; set the server's host to 127.0.0.1 or 0.0.0.0", host, taskGateway)
}

// isLocalhost reports whether a host is localhost, also written as the fully
// qualified localhost., with its trailing dot.
func isLocalhost(host string) bool {
	return strings.EqualFold(strings.TrimSuffix(host, "."), "localhost")
}

// CheckModelAddress refuses a model base URL that a harness in an Apptainer
// task could not reach. Inside the run, an address in the subnet of the run
// namespace or of a task namespace is slirp4netns's own or no one's, never a
// model server on the node's network. A managed server runs in the run
// namespace, whose loopback a task reaches only at 127.0.0.1, through its
// gateway: its URL must name 127.0.0.1, localhost or 0.0.0.0, over http, since
// the harness connects to the gateway's address, which a certificate for
// loopback would not name. A host name is not resolved.
func CheckModelAddress(baseURL string, managed bool) error {
	// A URL that does not parse is configuration validation's to report.
	var scheme, host string
	if parsed, err := url.Parse(baseURL); err == nil {
		scheme, host = parsed.Scheme, parsed.Hostname()
	}
	ip := net.ParseIP(host)
	if managed && (isLocalhost(host) || ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
		if scheme != "http" || !gatewayLeadsTo(host) {
			return fmt.Errorf("an Apptainer run's tasks cannot reach a managed model server at %s: they reach the run's loopback only at 127.0.0.1, through their gateway %s; set model.base_url to http on 127.0.0.1, localhost or 0.0.0.0 (https would need a certificate for %s), with the server listening on 127.0.0.1 or on all addresses", baseURL, taskGateway, taskGateway)
		}
		return nil
	}
	if ip == nil {
		return nil
	}
	for _, subnet := range []struct{ cidr, of string }{
		{runNetwork, "the run's network namespace"},
		{taskNetwork, "each task's network namespace"},
	} {
		_, network, err := net.ParseCIDR(subnet.cidr)
		if err == nil && network.Contains(ip) {
			return fmt.Errorf("an Apptainer run cannot reach a model server at %s: %s is the subnet slirp4netns gives %s; set model.base_url to another address of the server", ip, subnet.cidr, subnet.of)
		}
	}
	return nil
}

func (e *RunEnvironment) Start(context.Context) (core.RuntimePlacement, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.stopped {
		return core.RuntimePlacement{}, errors.New("run environment cannot be reused")
	}
	e.started = true
	if e.runID == "" {
		return core.RuntimePlacement{}, errors.New("run environment requires a run ID")
	}
	network, err := e.current()
	if err != nil {
		return core.RuntimePlacement{}, err
	}
	e.network = network
	return core.RuntimePlacement{AttachmentID: e.attachment()}, nil
}

func (e *RunEnvironment) attachment() string { return "run-namespace:" + e.network }

func (e *RunEnvironment) NewTaskEnvironment() deployment.TaskEnvironment {
	return &taskEnvironment{owner: e}
}

func (e *RunEnvironment) NetworkID() string { e.mu.Lock(); defer e.mu.Unlock(); return e.network }

func (e *RunEnvironment) NetworkName() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.network == "" {
		return ""
	}
	return e.attachment()
}

func (e *RunEnvironment) validate() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.network == "" || e.stopped {
		return errors.New("run environment is not active")
	}
	network, err := e.current()
	if err != nil {
		return err
	}
	if network != e.network {
		return errors.New("run left its network namespace")
	}
	return nil
}

// nextLoopback returns the loopback address for the next task namespace.
func (e *RunEnvironment) nextLoopback() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasks++
	return taskLoopback(e.tasks)
}

// Stop releases the environment. The namespace itself outlives it until the
// run's processes exit, and RunInNamespace's caller then removes nothing:
// a network namespace without processes is gone. Task handles end their own
// namespaces.
func (e *RunEnvironment) Stop(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	return nil
}

type taskEnvironment struct {
	mu               sync.Mutex
	owner            *RunEnvironment
	namespace        taskNamespaceHandle
	started, stopped bool
}

// Start gives the sandbox a network namespace of its own, which its harness
// and services share, and resolves the task's services on its loopback. Tasks
// running at once therefore do not compete for ports.
func (e *taskEnvironment) Start(ctx context.Context, request deployment.TaskEnvironmentRequest) (core.HarnessConnectivity, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.stopped {
		return core.HarnessConnectivity{}, errors.New("task environment cannot be reused")
	}
	e.started = true
	if request.RunID != e.owner.runID || request.RuntimeName == "" {
		return core.HarnessConnectivity{}, errors.New("task environment requires its run and a unique runtime name")
	}
	port := request.Environment.Services.SearchPort
	if port < 0 || port > 65535 {
		return core.HarnessConnectivity{}, errors.New("invalid task search service port")
	}
	for _, service := range request.Environment.Services.MCP {
		if service.Port <= 0 || service.Port > 65535 {
			return core.HarnessConnectivity{}, fmt.Errorf("invalid port for task MCP service %q", service.Name)
		}
	}
	if err := e.owner.validate(); err != nil {
		return core.HarnessConnectivity{}, err
	}
	loopback, err := e.owner.nextLoopback()
	if err != nil {
		return core.HarnessConnectivity{}, err
	}
	labels := map[string]string{
		"aries.managed": "true", "aries.kind": "task-network", "aries.component": "task-namespace",
		"aries.run": request.RunID, "aries.task": request.TaskID,
	}
	namespace, err := e.owner.startNamespace(ctx, taskNamespaceRequest{stateDir: e.owner.stateDir, runtime: request.RuntimeName, labels: labels, loopback: loopback})
	if namespace != nil {
		// Kept even when the start failed: a namespace that could not be
		// ended stays this handle's, for Stop to end.
		e.namespace = namespace
	}
	if err != nil {
		return core.HarnessConnectivity{}, fmt.Errorf("start the task's network namespace: %w", err)
	}
	result := core.HarnessConnectivity{
		Placement:    core.RuntimePlacement{AttachmentID: taskNamespacePrefix + namespace.Name()},
		ModelBaseURL: e.owner.harnessModel,
	}
	if port != 0 {
		result.SearchURL = fmt.Sprintf("http://%s:%d", loopbackHost, port)
	}
	for _, service := range request.Environment.Services.MCP {
		result.MCPServers = append(result.MCPServers, core.MCPServerConfig{
			Name: service.Name, URL: fmt.Sprintf("http://%s:%d%s", loopbackHost, service.Port, service.Path),
			Transport: service.Transport, TimeoutSeconds: service.TimeoutSeconds,
		})
	}
	return result, nil
}

func (e *taskEnvironment) Validate(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.stopped || e.namespace == nil {
		return errors.New("task environment is not active")
	}
	if err := e.owner.validate(); err != nil {
		return err
	}
	return e.namespace.Validate()
}

// Stop ends the task's namespace, after its runtimes are gone. It is safe to
// repeat, and to call after a Start that failed.
func (e *taskEnvironment) Stop(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.namespace != nil {
		if err := e.namespace.Stop(ctx); err != nil {
			return err
		}
	}
	e.stopped = true
	return nil
}
