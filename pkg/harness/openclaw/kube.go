package openclaw

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	gatewayclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

const (
	defaultHarnessNamespace = "aries"
	defaultKubectl          = "kubectl"
	sentinelPath            = "/run/aries/ready"
	// nodeRoleLabel is the label k8s/install puts on each dedicated node pool,
	// and the key of the NoSchedule taint it pairs with.
	nodeRoleLabel = "aries.dev/role"
	// defaultKubeStartTimeout is deliberately larger than the Docker backend's
	// 45s. On Docker the agent image is already local when Start runs; on
	// Kubernetes the kubelet pulls it on first use, inside this window, and a
	// cold pull of the OpenClaw image takes about a minute on a fresh node.
	// The Kubernetes sandbox uses 120s for the same reason.
	defaultKubeStartTimeout = 5 * time.Minute
)

// KubeOptions are the inputs to the Kubernetes-backed OpenClaw harness. ARIES
// runs out-of-cluster and drives the agent pod through the kubectl binary; the
// gateway is reached via a port-forward to a per-task Service.
type KubeOptions struct {
	Image     string
	OutputDir string
	Namespace string
	// NodeRole, when set, pins agent pods to nodes labelled
	// nodeRoleLabel=<NodeRole> and tolerates the matching NoSchedule taint.
	NodeRole       string
	KubectlPath    string
	APIKeyLookup   func(string) ([]byte, bool)
	StartTimeout   time.Duration
	AgentTimeout   time.Duration
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// KubeManager runs one OpenClaw agent pod per task. It reuses the package's
// config rendering, runtime archive staging, gateway client, and artifact
// helpers; only the container-runtime operations differ (kubectl + port-forward
// instead of the Docker SDK). Realtime voice mode is not supported by this
// backend.
type KubeManager struct {
	image          string
	outputDir      string
	namespace      string
	nodeRole       string
	kubectl        string
	apiKeyLookup   func(string) ([]byte, bool)
	startTimeout   time.Duration
	agentTimeout   time.Duration
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	newID          func() (string, error)

	mu       sync.Mutex
	active   *kubeSession
	stopping bool
}

type kubeSession struct {
	*session
	podName     string
	serviceName string
	portForward *exec.Cmd
}

var _ runner.AgentHarness = (*KubeManager)(nil)

// NewKube constructs a Kubernetes OpenClaw harness without contacting the
// cluster. The kubectl binary and its resolved kubeconfig/context are the
// cluster contract.
func NewKube(options KubeOptions) (*KubeManager, error) {
	if err := containerimage.ValidatePinnedTagOnly(options.Image); err != nil {
		return nil, fmt.Errorf("OpenClaw image: %w", err)
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("OpenClaw output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenClaw output directory: %w", err)
	}
	if err := ensurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare OpenClaw output directory: %w", err)
	}
	if options.Namespace == "" {
		options.Namespace = defaultHarnessNamespace
	}
	if options.KubectlPath == "" {
		options.KubectlPath = defaultKubectl
	}
	if options.StartTimeout <= 0 {
		options.StartTimeout = defaultKubeStartTimeout
	}
	if options.AgentTimeout <= 0 {
		options.AgentTimeout = defaultAgentTimeout
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	if options.APIKeyLookup == nil {
		options.APIKeyLookup = environmentAPIKeyLookup
	}
	return &KubeManager{
		image: options.Image, outputDir: outputDir, namespace: options.Namespace,
		nodeRole: options.NodeRole,
		kubectl:  options.KubectlPath, apiKeyLookup: options.APIKeyLookup,
		startTimeout: options.StartTimeout, agentTimeout: options.AgentTimeout,
		cleanupTimeout: options.CleanupTimeout, logger: options.Logger, newID: randomID,
	}, nil
}

// Close releases manager-level resources. The kubectl backend holds none.
func (manager *KubeManager) Close() error { return nil }

// Start creates the agent pod + Service, stages the per-task runtime, boots the
// gateway, and opens a port-forward the out-of-cluster ARIES uses to reach it.
func (manager *KubeManager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return errors.New("OpenClaw Kubernetes harness is already active")
	}
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if err := validateTaskID(request.TaskID); err != nil {
		return err
	}
	agentTimeout := request.Timeout
	if agentTimeout == 0 {
		agentTimeout = manager.agentTimeout
	}
	configuration, err := renderConfig(request.Model, request.Endpoint)
	if err != nil {
		return err
	}
	apiKeySource, ok := manager.apiKeyLookup(request.Model.APIKeyEnv)
	if !ok {
		clear(apiKeySource)
		return fmt.Errorf("OpenClaw API-key environment %q is not set", request.Model.APIKeyEnv)
	}
	apiKey := bytes.Clone(apiKeySource)
	clear(apiKeySource)
	if err := validateAPIKey(apiKey); err != nil {
		clear(apiKey)
		return err
	}
	if bytes.Contains(configuration, apiKey) {
		clear(apiKey)
		return errors.New("rendered OpenClaw config contains the API-key value")
	}
	id, err := manager.newID()
	if err != nil {
		clear(apiKey)
		return fmt.Errorf("generate OpenClaw harness ID: %w", err)
	}
	gatewayToken, err := randomSecret(32)
	if err != nil {
		clear(apiKey)
		return fmt.Errorf("generate OpenClaw gateway token: %w", err)
	}
	agentIdempotency, err := randomID()
	if err != nil {
		clear(apiKey)
		clear(gatewayToken)
		return fmt.Errorf("generate OpenClaw agent idempotency key: %w", err)
	}
	active := &kubeSession{
		session: &session{
			runID: request.RunID, taskID: request.TaskID, safeTaskID: safeTaskID(request.TaskID), attemptID: id,
			containerName: "aries-openclaw-" + id, artifactDir: filepath.Join(manager.outputDir, request.TaskID, "harness"),
			endpoint: request.Endpoint, model: request.Model, agentTimeout: agentTimeout,
			apiKey: apiKey, gatewayToken: gatewayToken, agentIdempotency: agentIdempotency,
		},
		podName:     "aries-openclaw-" + id,
		serviceName: "aries-openclaw-" + id,
	}
	active.containerID = manager.namespace + "/" + active.podName

	fail := func(primary error) error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
		defer cancel()
		cleanupErr := manager.teardown(cleanupCtx, active)
		clearSessionSecrets(active.session)
		if cleanupErr != nil {
			return errors.Join(primary, fmt.Errorf("rollback partial OpenClaw Kubernetes harness: %w", cleanupErr))
		}
		_ = os.RemoveAll(active.artifactDir)
		return primary
	}

	if err := ensurePrivateDirectory(active.artifactDir); err != nil {
		return fail(fmt.Errorf("create OpenClaw artifact directory: %w", err))
	}
	configArtifact := filepath.Join(active.artifactDir, "openclaw.json")
	if err := writeArtifact(configArtifact, configuration); err != nil {
		return fail(fmt.Errorf("retain rendered OpenClaw config: %w", err))
	}
	active.logPaths = appendUnique(active.logPaths, configArtifact)

	archive, err := buildRuntimeArchive(active.session, configuration, "")
	if err != nil {
		return fail(err)
	}
	defer clear(archive)

	// Create the Service then the Pod. The pod idles until the runtime is staged.
	if err := manager.apply(ctx, servicePodManifest(active, manager.namespace)); err != nil {
		return fail(fmt.Errorf("apply OpenClaw Service: %w", err))
	}
	if err := manager.apply(ctx, podManifest(active, manager.namespace, manager.image, manager.nodeRole)); err != nil {
		return fail(fmt.Errorf("apply OpenClaw pod: %w", err))
	}

	startCtx, cancel := context.WithTimeout(ctx, manager.startTimeout)
	defer cancel()
	// Wait for the container to be running so exec can stage the runtime.
	if _, err := manager.run(startCtx, "wait", "-n", manager.namespace,
		"--for=jsonpath={.status.phase}=Running", "pod/"+active.podName,
		"--timeout="+seconds(manager.startTimeout)); err != nil {
		return fail(fmt.Errorf("wait for OpenClaw pod Running: %w", err))
	}
	// Stage the private runtime archive, then release the gateway.
	// --no-same-owner/--no-overwrite-dir let a non-root extraction reuse the
	// pre-existing volume mountpoints without trying to chown/chmod them.
	if _, err := manager.runInput(startCtx, archive, "exec", "-i", "-n", manager.namespace, active.podName,
		"--", "tar", "-xmf", "-", "-C", "/", "--no-same-owner", "--no-overwrite-dir"); err != nil {
		return fail(fmt.Errorf("stage OpenClaw runtime into pod: %w", err))
	}
	if _, err := manager.run(startCtx, "exec", "-n", manager.namespace, active.podName,
		"--", "/bin/sh", "-c", "touch "+sentinelPath); err != nil {
		return fail(fmt.Errorf("release OpenClaw gateway: %w", err))
	}
	if err := manager.waitReady(startCtx, active); err != nil {
		return fail(err)
	}
	localPort, err := manager.startPortForward(ctx, active)
	if err != nil {
		return fail(err)
	}
	active.gatewayURL = "ws://127.0.0.1:" + localPort

	manager.active = active
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.taskID, "pod": active.podName}).Info("OpenClaw Kubernetes harness started")
	return nil
}

// Run drives the agent over the port-forwarded gateway. Mirrors the Docker
// backend's agent path (realtime mode is unsupported here).
func (manager *KubeManager) Run(ctx context.Context, instruction string) (core.HarnessResult, error) {
	started := time.Now()
	manager.mu.Lock()
	active := manager.active
	if active == nil {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("OpenClaw Kubernetes harness is not started")
	}
	if active.runAttempted {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("OpenClaw harness accepts exactly one task instruction")
	}
	if strings.TrimSpace(instruction) == "" || strings.ContainsRune(instruction, 0) {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("OpenClaw task instruction is invalid")
	}
	active.runAttempted = true
	manager.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, active.agentTimeout)
	client, err := newGatewayClientWithDisposition(active.gatewayURL, active.gatewayToken, gatewayScopes(ModeAgent), gatewayEventDisposition(ModeAgent))
	if err != nil {
		cancel()
		err = redactSessionError(err, active.session)
		return failedHarnessResult(active.session, started, err), err
	}
	defer client.Close()
	connectSummary, err := client.Connect(runCtx, gatewayclient.ConnectOptions{})
	var agentResult gatewayclient.AgentResult
	if err == nil && !connectSummary.HasScope("operator.write") {
		err = errors.New("OpenClaw agent gateway requires operator.write scope")
	}
	if err == nil {
		thinking := ""
		if disablesThinking(active.model) {
			thinking = "off"
		}
		agentResult, err = client.Agent(runCtx, gatewayclient.AgentRequest{
			Message: instruction, SessionKey: "agent:main:aries-" + active.safeTaskID,
			IdempotencyKey: active.agentIdempotency, Thinking: thinking,
		})
	}
	cancel()
	connectSummary = redactConnectSummary(connectSummary, active.session)
	agentResult = redactAgentResult(agentResult, active.session)
	err = redactSessionError(err, active.session)
	resultPath, writeErr := writeAgentResult(active.session, connectSummary, agentResult, err)
	if writeErr == nil {
		active.logPaths = appendUnique(active.logPaths, resultPath)
	}
	err = errors.Join(err, writeErr)
	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	artifactErr := manager.collectArtifacts(artifactCtx, active)
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		return failedHarnessResult(active.session, started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: agentResult.Text, Duration: time.Since(started),
		LogPaths: append([]string(nil), active.logPaths...),
	}, nil
}

// Stop tears down the pod, Service, and port-forward and confirms removal.
func (manager *KubeManager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	active := manager.active
	if active == nil {
		manager.mu.Unlock()
		return nil
	}
	manager.stopping = true
	manager.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	defer cancel()
	err := manager.teardown(cleanupCtx, active)
	clearSessionSecrets(active.session)

	manager.mu.Lock()
	manager.stopping = false
	if err == nil {
		manager.active = nil
	}
	manager.mu.Unlock()
	return err
}

func (manager *KubeManager) teardown(ctx context.Context, active *kubeSession) error {
	var errs []error
	if active.portForward != nil && active.portForward.Process != nil {
		_ = active.portForward.Process.Kill()
		_ = active.portForward.Wait()
		active.portForward = nil
	}
	if _, err := manager.run(ctx, "delete", "pod", "-n", manager.namespace, active.podName, "--ignore-not-found", "--now"); err != nil {
		errs = append(errs, fmt.Errorf("delete OpenClaw pod: %w", err))
	}
	if _, err := manager.run(ctx, "delete", "service", "-n", manager.namespace, active.serviceName, "--ignore-not-found"); err != nil {
		errs = append(errs, fmt.Errorf("delete OpenClaw Service: %w", err))
	}
	if out, err := manager.run(ctx, "get", "pod", "-n", manager.namespace, active.podName, "--ignore-not-found", "-o", "name"); err != nil {
		errs = append(errs, fmt.Errorf("confirm OpenClaw pod absent: %w", err))
	} else if strings.TrimSpace(string(out)) != "" {
		errs = append(errs, fmt.Errorf("OpenClaw pod %q still present after delete", active.podName))
	}
	return errors.Join(errs...)
}

// waitReady polls the in-pod gateway /readyz until it reports ready as uid 1000.
func (manager *KubeManager) waitReady(ctx context.Context, active *kubeSession) error {
	probe := `const http=require("http");const port=Number(process.argv[1]);const r=http.get({host:"127.0.0.1",port,path:"/readyz",timeout:1000},s=>{let b="";s.on("data",c=>b+=c);s.on("end",()=>{try{const j=JSON.parse(b);process.exit(s.statusCode===200&&j.ready===true&&process.getuid()===1000?0:1)}catch{process.exit(1)}})});r.on("timeout",()=>r.destroy());r.on("error",()=>process.exit(1));`
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := manager.run(probeCtx, "exec", "-n", manager.namespace, active.podName, "--", "node", "-e", probe, upstreamGatewayPort)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await OpenClaw gateway readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// startPortForward opens `kubectl port-forward svc/<name> :18789` and returns
// the chosen local port once kubectl reports it is forwarding.
func (manager *KubeManager) startPortForward(ctx context.Context, active *kubeSession) (string, error) {
	cmd := exec.Command(manager.kubectl, "port-forward", "-n", manager.namespace,
		"service/"+active.serviceName, ":"+gatewayListenPort)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("pipe OpenClaw port-forward: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start OpenClaw port-forward: %w", err)
	}
	active.portForward = cmd
	// Parse "Forwarding from 127.0.0.1:<port> -> 18789".
	portCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if idx := strings.Index(line, "127.0.0.1:"); idx >= 0 {
				rest := line[idx+len("127.0.0.1:"):]
				if end := strings.IndexByte(rest, ' '); end > 0 {
					portCh <- rest[:end]
					return
				}
			}
		}
		portCh <- ""
	}()
	select {
	case port := <-portCh:
		if port == "" {
			return "", errors.New("OpenClaw port-forward did not report a local port")
		}
		return port, nil
	case <-time.After(15 * time.Second):
		return "", errors.New("timed out waiting for OpenClaw port-forward")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// collectArtifacts saves the pod's gateway logs and a telemetry index.
func (manager *KubeManager) collectArtifacts(ctx context.Context, active *kubeSession) error {
	var errs []error
	if logs, err := manager.run(ctx, "logs", "-n", manager.namespace, active.podName); err != nil {
		errs = append(errs, fmt.Errorf("collect OpenClaw gateway logs: %w", err))
	} else {
		content := allowGatewayLogs(logs, active.apiKey, active.gatewayToken)
		path := filepath.Join(active.artifactDir, "gateway.log")
		if err := writeArtifact(path, content); err != nil {
			errs = append(errs, err)
		} else {
			active.logPaths = appendUnique(active.logPaths, path)
		}
	}
	index, err := json.MarshalIndent(struct {
		Paths []string `json:"paths"`
	}{Paths: telemetryRelativePaths(active.artifactDir, active.logPaths)}, "", "  ")
	if err == nil {
		index = append(index, '\n')
		path := filepath.Join(active.artifactDir, "telemetry.index.json")
		if err = writeArtifact(path, index); err == nil {
			active.logPaths = appendUnique(active.logPaths, path)
		}
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("write OpenClaw telemetry index: %w", err))
	}
	return errors.Join(errs...)
}

// --- kubectl plumbing -------------------------------------------------------

func (manager *KubeManager) run(ctx context.Context, args ...string) ([]byte, error) {
	return manager.runInput(ctx, nil, args...)
}

func (manager *KubeManager) runInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, manager.kubectl, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (manager *KubeManager) apply(ctx context.Context, manifest []byte) error {
	_, err := manager.runInput(ctx, manifest, "apply", "-f", "-")
	return err
}

func seconds(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

func podManifest(active *kubeSession, namespace, image, nodeRole string) []byte {
	boot := "while [ ! -f " + sentinelPath + " ]; do sleep 0.3; done; exec " + launcherPath + " " + gatewayLauncherPath
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": active.podName, "namespace": namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name": "aries-openclaw", "app.kubernetes.io/component": "harness",
				// managed-by is the ownership label the resource source selects
				// on. Without it the agent pod is invisible to pod telemetry and
				// only sandbox readings reach resources.jsonl.
				"app.kubernetes.io/managed-by": "aries",
				"aries.dev/attempt":            active.attemptID,
			},
			// Run/task IDs can exceed the 63-byte label limit; keep them as
			// annotations. The short attempt ID stays a label for Service
			// selection.
			"annotations": map[string]string{
				"aries.dev/run-id": active.runID, "aries.dev/task-id": active.taskID,
			},
		},
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"automountServiceAccountToken": false,
			// The container runs as uid 1000 (readiness requires it), so it
			// cannot write under root-owned /run or /opt. Mount writable
			// emptyDir volumes at the two staging roots and have a root
			// initContainer chown them to 1000 so `kubectl exec tar` (running as
			// the node user) can create and chmod the runtime files there.
			"securityContext": map[string]any{"fsGroup": 1000},
			"initContainers": []any{map[string]any{
				"name": "fix-perms", "image": image,
				"command":         []string{"/bin/sh", "-c", "chown 1000:1000 /run/aries /opt/aries"},
				"securityContext": map[string]any{"runAsUser": 0},
				"volumeMounts": []any{
					map[string]any{"name": "run-aries", "mountPath": "/run/aries"},
					map[string]any{"name": "opt-aries", "mountPath": "/opt/aries"},
				},
			}},
			"containers": []any{map[string]any{
				"name": "openclaw", "image": image,
				"command": []string{"/bin/sh", "-c", boot},
				"env":     []any{map[string]string{"name": "OPENCLAW_CONFIG_PATH", "value": configContainerPath}},
				"ports":   []any{map[string]any{"name": "gateway", "containerPort": 18789}},
				"volumeMounts": []any{
					map[string]any{"name": "run-aries", "mountPath": "/run/aries"},
					map[string]any{"name": "opt-aries", "mountPath": "/opt/aries"},
				},
			}},
			"volumes": []any{
				map[string]any{"name": "run-aries", "emptyDir": map[string]any{}},
				map[string]any{"name": "opt-aries", "emptyDir": map[string]any{}},
			},
		},
	}
	// Pin the agent pod to its dedicated node pool. Both halves are required:
	// the nodeSelector pulls the pod onto a labelled node, the toleration gets
	// it past the NoSchedule taint keeping everything else off. An empty role
	// omits both, so a cluster with no role labels still schedules agent pods.
	if nodeRole != "" {
		spec := pod["spec"].(map[string]any)
		spec["nodeSelector"] = map[string]string{nodeRoleLabel: nodeRole}
		spec["tolerations"] = []any{map[string]any{
			"key":      nodeRoleLabel,
			"operator": "Equal",
			"value":    nodeRole,
			"effect":   "NoSchedule",
		}}
	}
	out, _ := json.Marshal(pod)
	return out
}

func servicePodManifest(active *kubeSession, namespace string) []byte {
	service := map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{
			"name": active.serviceName, "namespace": namespace,
			"labels": map[string]string{"app.kubernetes.io/name": "aries-openclaw", "aries.dev/task-id": active.taskID},
		},
		"spec": map[string]any{
			"selector": map[string]string{"app.kubernetes.io/name": "aries-openclaw", "aries.dev/attempt": active.attemptID},
			"ports":    []any{map[string]any{"name": "gateway", "port": 18789, "targetPort": "gateway"}},
		},
	}
	out, _ := json.Marshal(service)
	return out
}
