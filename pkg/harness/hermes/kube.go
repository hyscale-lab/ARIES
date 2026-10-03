package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

const (
	defaultKubeNamespace = "aries"
	defaultKubectl       = "kubectl"
	// nodeRoleLabel is the label k8s/setup/ puts on each dedicated node pool, and
	// the key of the NoSchedule taint it pairs with.
	nodeRoleLabel = "aries.dev/role"
	// defaultKubeStartTimeout is larger than the Docker backend's 45s because
	// the kubelet pulls the image inside this window, and the Hermes image is
	// several gigabytes on a cold node. OpenClaw's Kubernetes backend and the
	// Kubernetes sandbox size theirs the same way.
	defaultKubeStartTimeout = 5 * time.Minute
	kubeContainerName       = "hermes"
	kubeGracePeriodSeconds  = gracefulStopSeconds
)

// kubeIdleCommand keeps the pod alive without the upstream s6 init, so ARIES
// owns when the agent starts, as idleCommand does on Docker. It needs no chown:
// staging runs after start as root through GNU tar, which keeps the archive's
// hermes ownership, whereas Docker's copy API resets it and so must chown.
var kubeIdleCommand = []string{"/bin/sh", "-c", "exec sleep infinity"}

// kubeExecShell is execShell with the working directory as its first argument,
// because `kubectl exec` has no equivalent of Docker's exec WorkingDir.
const kubeExecShell = `cd "$1" || exit 1
shift
token=$1
shift
"$@"
status=$?
printf '\036ARIES_HERMES_EXIT_%s=%s\037' "$token" "$status" >&2
exit "$status"`

// kubectlExitSuffix is what kubectl itself appends to stderr when the remote
// command exits non-zero. It shares a stream with the exit trailer, so it must
// be removed before the trailer is parsed; anything else after the trailer is
// treated as corruption.
var kubectlExitSuffix = regexp.MustCompile(`command terminated with exit code \d+\n?$`)

// KubeOptions are the inputs to the Kubernetes-backed Hermes harness. ARIES
// drives the agent pod through the kubectl binary; the resolved kubeconfig and
// context are the cluster contract.
type KubeOptions struct {
	Image     string
	OutputDir string
	Namespace string
	// NodeRole, when set, pins agent pods to nodes labelled
	// nodeRoleLabel=<NodeRole> and tolerates the matching NoSchedule taint.
	NodeRole    string
	KubectlPath string
	// APIKeyLookup has the same ownership contract as Options.APIKeyLookup.
	APIKeyLookup    func(string) ([]byte, bool)
	MaxTurns        int
	TerminalTimeout int
	StartTimeout    time.Duration
	AgentTimeout    time.Duration
	CleanupTimeout  time.Duration
	Logger          *logrus.Logger
}

// kubectlRunner runs one kubectl invocation with optional stdin and returns its
// stdout and stderr separately. err carries a non-zero exit status.
type kubectlRunner interface {
	Run(ctx context.Context, stdin []byte, args ...string) (stdout, stderr []byte, err error)
}

// KubeManager runs one Hermes agent pod per task. The agent is the same one-shot
// the Docker backend runs, executed in an idling pod, so Hermes needs no Service
// and no port-forward: its only network peers are the model API and the
// hermes-ssh bridge, which it dials itself.
type KubeManager struct {
	image           string
	outputDir       string
	namespace       string
	nodeRole        string
	kubectl         kubectlRunner
	apiKeyLookup    func(string) ([]byte, bool)
	maxTurns        int
	terminalTimeout int
	startTimeout    time.Duration
	agentTimeout    time.Duration
	cleanupTimeout  time.Duration
	logger          *logrus.Logger
	newID           func() (string, error)

	mu       sync.Mutex
	active   *kubeSession
	stopping bool
}

type kubeSession struct {
	*session
	podName string
}

var _ runner.AgentHarness = (*KubeManager)(nil)

// NewKube constructs a Kubernetes Hermes harness without contacting the cluster.
func NewKube(options KubeOptions) (*KubeManager, error) {
	if err := containerimage.ValidatePinnedTagOnly(options.Image); err != nil {
		return nil, fmt.Errorf("Hermes image: %w", err)
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("Hermes output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Hermes output directory: %w", err)
	}
	if err := ensurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare Hermes output directory: %w", err)
	}
	if options.Namespace == "" {
		options.Namespace = defaultKubeNamespace
	}
	if !dnsLabelPattern.MatchString(options.Namespace) {
		return nil, fmt.Errorf("Hermes namespace %q is invalid", options.Namespace)
	}
	if options.NodeRole != "" && !labelValuePattern.MatchString(options.NodeRole) {
		return nil, fmt.Errorf("Hermes node role %q is not a Kubernetes label value", options.NodeRole)
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
	if options.MaxTurns <= 0 {
		options.MaxTurns = defaultMaxTurns
	}
	if options.TerminalTimeout <= 0 {
		options.TerminalTimeout = defaultTerminalTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	if options.APIKeyLookup == nil {
		options.APIKeyLookup = environmentAPIKeyLookup
	}
	return &KubeManager{
		image: options.Image, outputDir: outputDir, namespace: options.Namespace, nodeRole: options.NodeRole,
		kubectl: execKubectl{path: options.KubectlPath}, apiKeyLookup: options.APIKeyLookup,
		maxTurns: options.MaxTurns, terminalTimeout: options.TerminalTimeout,
		startTimeout: options.StartTimeout, agentTimeout: options.AgentTimeout,
		cleanupTimeout: options.CleanupTimeout, logger: options.Logger, newID: randomID,
	}, nil
}

// Close releases manager-level resources. The kubectl backend holds none.
func (manager *KubeManager) Close() error { return nil }

// Start creates the agent pod, verifies what the API server admitted, stages the
// private runtime, and waits until the Hermes CLI answers from it.
func (manager *KubeManager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return errors.New("Hermes Kubernetes harness is already active")
	}
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if err := validateTaskID(request.TaskID); err != nil {
		return err
	}
	if request.Timeout < 0 {
		return errors.New("Hermes task timeout must not be negative")
	}
	limits, err := kubeResourceLimits(request)
	if err != nil {
		return err
	}
	agentTimeout := request.Timeout
	if agentTimeout == 0 {
		agentTimeout = manager.agentTimeout
	}
	configuration, err := renderConfig(request.Model, manager.maxTurns)
	if err != nil {
		return err
	}
	environment, err := containerEnvironment(request.Endpoint, workspaceRoot, manager.terminalTimeout)
	if err != nil {
		return err
	}
	apiKeySource, ok := manager.apiKeyLookup(request.Model.APIKeyEnv)
	if !ok {
		clear(apiKeySource)
		return fmt.Errorf("Hermes API-key environment %q is not set", request.Model.APIKeyEnv)
	}
	apiKey := bytes.Clone(apiKeySource)
	clear(apiKeySource)
	if err := validateAPIKey(apiKey); err != nil {
		clear(apiKey)
		return err
	}
	if bytes.Contains(configuration, apiKey) {
		clear(apiKey)
		return errors.New("rendered Hermes config contains the API-key value")
	}
	id, err := manager.newID()
	if err != nil {
		clear(apiKey)
		return fmt.Errorf("generate Hermes harness ID: %w", err)
	}
	active := &kubeSession{
		session: &session{
			runID: request.RunID, taskID: request.TaskID, attemptID: id,
			containerName: "aries-hermes-" + id,
			artifactDir:   filepath.Join(manager.outputDir, request.TaskID, "harness"),
			endpoint:      request.Endpoint, model: request.Model,
			agentTimeout: agentTimeout, apiKey: apiKey,
		},
		podName: "aries-hermes-" + id,
	}
	active.containerID = manager.namespace + "/" + active.podName

	fail := func(primary error) error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
		defer cancel()
		cleanupErr := manager.teardown(cleanupCtx, active)
		clearSessionSecrets(active.session)
		if cleanupErr != nil {
			return errors.Join(primary, fmt.Errorf("rollback partial Hermes Kubernetes harness: %w", cleanupErr))
		}
		_ = os.RemoveAll(active.artifactDir)
		return primary
	}

	if err := ensurePrivateDirectory(active.artifactDir); err != nil {
		return fail(fmt.Errorf("create Hermes artifact directory: %w", err))
	}
	configArtifact := filepath.Join(active.artifactDir, "config.yaml")
	if err := writeArtifact(configArtifact, redactSession(configuration, active.session)); err != nil {
		return fail(fmt.Errorf("retain rendered Hermes config: %w", err))
	}
	active.logPaths = appendUnique(active.logPaths, configArtifact)
	archive, err := buildRuntimeArchive(active.session, configuration)
	if err != nil {
		return fail(err)
	}
	defer clear(archive)

	manifest, err := hermesPodManifest(active, manager.namespace, manager.image, manager.nodeRole, environment, limits)
	if err != nil {
		return fail(err)
	}
	// create, not apply: the attempt ID is fresh, so an existing pod of this
	// name is someone else's and must not be patched into ours.
	if _, _, err := manager.kubectl.Run(ctx, manifest, "create", "-f", "-"); err != nil {
		return fail(fmt.Errorf("create Hermes pod: %w", err))
	}

	startCtx, cancel := context.WithTimeout(ctx, manager.startTimeout)
	defer cancel()
	if _, _, err := manager.kubectl.Run(startCtx, nil, "wait", "-n", manager.namespace,
		"--for=jsonpath={.status.phase}=Running", "pod/"+active.podName,
		"--timeout="+strconv.Itoa(int(manager.startTimeout.Seconds()))+"s"); err != nil {
		return fail(fmt.Errorf("wait for Hermes pod Running: %w", err))
	}
	if err := manager.verifyPod(startCtx, active); err != nil {
		return fail(err)
	}
	// The pod runs as root, so GNU tar keeps the archive's hermes ownership
	// and modes; --same-owner makes that explicit rather than a default.
	if _, _, err := manager.kubectl.Run(startCtx, archive, "exec", "-i", "-n", manager.namespace, active.podName,
		"-c", kubeContainerName, "--", "tar", "-xpf", "-", "-C", "/", "--same-owner"); err != nil {
		return fail(fmt.Errorf("stage private Hermes runtime: %w", redactSessionError(err, active.session)))
	}
	err = awaitReady(startCtx,
		func(probeCtx context.Context) (execResult, error) {
			return manager.exec(probeCtx, active, []string{"/bin/sh", "-c", readinessProbe}, workspaceRoot)
		},
		func(ctx context.Context) error { return manager.podRunning(ctx, active) })
	if err != nil {
		return fail(err)
	}
	manager.active = active
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.taskID, "pod": active.podName}).Info("Hermes Kubernetes harness started")
	return nil
}

// Run executes the Hermes one-shot in the pod. It mirrors the Docker backend's
// Run, including the artifact layout, so results from either backend compare.
func (manager *KubeManager) Run(ctx context.Context, instruction string) (core.HarnessResult, error) {
	started := time.Now()
	manager.mu.Lock()
	active := manager.active
	if active == nil {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Hermes Kubernetes harness is not started")
	}
	if active.runAttempted {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Hermes harness accepts exactly one task instruction")
	}
	if strings.TrimSpace(instruction) == "" || strings.ContainsRune(instruction, 0) {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Hermes task instruction is invalid")
	}
	active.runAttempted = true
	// As on Docker, Run outlives the lock, so it works from its own copy of the
	// session rather than the one a concurrent Stop may be clearing.
	localSession := *active.session
	local := &kubeSession{session: &localSession, podName: active.podName}
	manager.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, local.agentTimeout)
	result, runErr := manager.exec(runCtx, local,
		[]string{agentWrapperPath, local.model.Model, local.model.Provider, instruction}, workspaceRoot)
	cancel()

	stdout := redactSession(result.stdout, local.session)
	stderr := redactSession(result.stderr, local.session)
	err := runErr
	if err == nil && result.exitCode != 0 {
		err = fmt.Errorf("Hermes one-shot exited with status %d", result.exitCode)
	}
	outcome := newRunOutcome(started, result.exitCode, runErr)
	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	artifactErr := retainRunArtifacts(artifactCtx, manager.logger, local.session, stdout, stderr, outcome, artifactSources{
		containerLogs: func(ctx context.Context) ([]byte, error) {
			logs, _, err := manager.kubectl.Run(ctx, nil, "logs", "-n", manager.namespace, local.podName, "-c", kubeContainerName)
			if err != nil {
				return nil, fmt.Errorf("collect Hermes pod logs: %w", err)
			}
			return logs, nil
		},
		exportSessions: func(ctx context.Context) (execResult, error) {
			return manager.exec(ctx, local, []string{"hermes", "sessions", "export", "-"}, workspaceRoot)
		},
	})
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		err = redactSessionError(err, local.session)
		return failedHarnessResult(local.session, started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: strings.TrimRight(string(stdout), "\n"),
		Duration: time.Since(started), LogPaths: append([]string(nil), local.logPaths...),
	}, nil
}

// Stop deletes the pod and confirms it is gone. A failed teardown leaves the
// session active so Stop can be retried.
func (manager *KubeManager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	active := manager.active
	if active == nil {
		manager.mu.Unlock()
		return nil
	}
	if manager.stopping {
		manager.mu.Unlock()
		return errors.New("Hermes Kubernetes harness is already stopping")
	}
	manager.stopping = true
	manager.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	defer cancel()
	err := manager.teardown(cleanupCtx, active)

	manager.mu.Lock()
	manager.stopping = false
	if err == nil {
		clearSessionSecrets(active.session)
		manager.active = nil
	}
	manager.mu.Unlock()
	return err
}

// teardown deletes the pod and confirms absence. Deleting the pod also ends any
// agent process a cancelled `kubectl exec` left running inside it, since killing
// the kubectl client does not reliably stop the remote process.
func (manager *KubeManager) teardown(ctx context.Context, active *kubeSession) error {
	var errs []error
	if _, _, err := manager.kubectl.Run(ctx, nil, "delete", "pod", "-n", manager.namespace, active.podName,
		"--ignore-not-found", "--wait=true", "--grace-period="+strconv.Itoa(kubeGracePeriodSeconds)); err != nil {
		errs = append(errs, fmt.Errorf("delete Hermes pod: %w", err))
	}
	out, _, err := manager.kubectl.Run(ctx, nil, "get", "pod", "-n", manager.namespace, active.podName, "--ignore-not-found", "-o", "name")
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("confirm Hermes pod absent: %w", err))
	case strings.TrimSpace(string(out)) != "":
		errs = append(errs, fmt.Errorf("Hermes pod %q still present after delete", active.podName))
	}
	return errors.Join(errs...)
}

func (manager *KubeManager) podRunning(ctx context.Context, active *kubeSession) error {
	phase, _, err := manager.kubectl.Run(ctx, nil, "get", "pod", "-n", manager.namespace, active.podName, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return fmt.Errorf("inspect Hermes readiness: %w", err)
	}
	if strings.TrimSpace(string(phase)) != "Running" {
		return errors.New("Hermes pod exited before readiness")
	}
	return nil
}

// exec runs command in the pod and recovers its exit status from the trailer.
//
// kubectl reports a non-zero remote exit as its own error, and appends
// "command terminated with exit code N" to the stderr stream the trailer is on.
// A task the agent fails is still a successful exec, so when the trailer parses
// the kubectl error is disregarded; only a missing or corrupt trailer means the
// exec itself failed.
func (manager *KubeManager) exec(ctx context.Context, active *kubeSession, command []string, workdir string) (execResult, error) {
	token, err := randomID()
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("generate Hermes exec token: %w", err)
	}
	args := append([]string{"exec", "-n", manager.namespace, active.podName, "-c", kubeContainerName, "--",
		"/bin/sh", "-c", kubeExecShell, "aries-hermes-exec", workdir, token}, command...)
	stdout, stderr, runErr := manager.kubectl.Run(ctx, nil, args...)
	return parseKubeExec(ctx, token, stdout, stderr, runErr)
}

func parseKubeExec(ctx context.Context, token string, stdout, stderr []byte, runErr error) (execResult, error) {
	if len(stdout) > maxDockerOutput || len(stderr) > maxDockerOutput {
		return execResult{stdout: stdout, stderr: stderr, exitCode: -1}, errors.New("Hermes exec output exceeded its bound")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return execResult{stdout: stdout, stderr: stderr, exitCode: -1}, ctxErr
	}
	trimmed := stderr
	if end := bytes.LastIndexByte(stderr, '\x1f'); end >= 0 {
		rest := stderr[end+1:]
		if len(rest) != 0 && !kubectlExitSuffix.Match(rest) {
			return execResult{stdout: stdout, stderr: stderr, exitCode: -1}, errors.New("Hermes exec output has data after its exit trailer")
		}
		trimmed = stderr[:end+1]
	}
	var remainder limitedBuffer
	remainder.limit = maxDockerOutput
	trailer := newExecTrailer(&remainder, token)
	if _, err := trailer.Write(trimmed); err != nil {
		return execResult{stdout: stdout, stderr: stderr, exitCode: -1}, err
	}
	exitCode, err := trailer.Finish()
	if err != nil {
		// No trailer means the command never reported back: the exec failed.
		return execResult{stdout: stdout, stderr: stderr, exitCode: -1}, errors.Join(err, runErr)
	}
	return execResult{stdout: stdout, stderr: remainder.Bytes(), exitCode: exitCode}, nil
}

// verifyPod checks what the API server actually admitted, which a mutating
// admission webhook may have changed from what ARIES created. It is the
// Kubernetes counterpart of the Docker backend's validateContainer: the agent
// must run only the pinned image and idle command, carry no credential in its
// spec, and have no route to host state.
func (manager *KubeManager) verifyPod(ctx context.Context, active *kubeSession) error {
	raw, _, err := manager.kubectl.Run(ctx, nil, "get", "pod", "-n", manager.namespace, active.podName, "-o", "json")
	if err != nil {
		return fmt.Errorf("inspect Hermes pod: %w", err)
	}
	return checkAdmittedPod(raw, active.session, manager.image)
}

type admittedPod struct {
	Metadata struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		HostNetwork                  bool              `json:"hostNetwork"`
		HostPID                      bool              `json:"hostPID"`
		HostIPC                      bool              `json:"hostIPC"`
		AutomountServiceAccountToken *bool             `json:"automountServiceAccountToken"`
		InitContainers               []json.RawMessage `json:"initContainers"`
		EphemeralContainers          []json.RawMessage `json:"ephemeralContainers"`
		Containers                   []struct {
			Name    string   `json:"name"`
			Image   string   `json:"image"`
			Command []string `json:"command"`
			Args    []string `json:"args"`
			Env     []struct {
				Name      string          `json:"name"`
				Value     string          `json:"value"`
				ValueFrom json.RawMessage `json:"valueFrom"`
			} `json:"env"`
			VolumeMounts    []json.RawMessage `json:"volumeMounts"`
			SecurityContext *struct {
				Privileged *bool `json:"privileged"`
			} `json:"securityContext"`
		} `json:"containers"`
		Volumes []json.RawMessage `json:"volumes"`
	} `json:"spec"`
}

func checkAdmittedPod(raw []byte, active *session, image string) error {
	var pod admittedPod
	if err := json.Unmarshal(raw, &pod); err != nil {
		return fmt.Errorf("decode Hermes pod: %w", err)
	}
	if containsSecret(string(raw), active.apiKey) {
		return errors.New("Hermes secret entered the pod spec")
	}
	labels := pod.Metadata.Labels
	if labels["app.kubernetes.io/managed-by"] != "aries" || labels["app.kubernetes.io/component"] != "harness" ||
		labels["aries.dev/attempt"] != active.attemptID ||
		pod.Metadata.Annotations["aries.dev/run-id"] != active.runID || pod.Metadata.Annotations["aries.dev/task-id"] != active.taskID {
		return errors.New("Hermes pod labels do not match the task")
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		return errors.New("Hermes pod must not share the node's network, PID or IPC namespace")
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		return errors.New("Hermes pod must not mount a service account token")
	}
	// Any volume at all is a route to state ARIES did not stage: a hostPath,
	// a projected token, or a secret. The pod is created with none.
	if len(pod.Spec.Volumes) != 0 {
		return errors.New("Hermes pod must not carry volumes")
	}
	// A second container, such as an injected sidecar, would share the pod's
	// network and could observe the agent's traffic to the model API.
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) != 0 || len(pod.Spec.EphemeralContainers) != 0 {
		return errors.New("Hermes pod must run exactly the Hermes container")
	}
	container := pod.Spec.Containers[0]
	if container.Name != kubeContainerName || container.Image != image ||
		!slices.Equal(container.Command, kubeIdleCommand) || len(container.Args) != 0 {
		return errors.New("Hermes image or idle command differs from the pinned configuration")
	}
	if len(container.VolumeMounts) != 0 {
		return errors.New("Hermes container must not mount volumes")
	}
	if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		return errors.New("Hermes container must not be privileged")
	}
	for _, variable := range container.Env {
		if len(variable.ValueFrom) != 0 && string(variable.ValueFrom) != "null" {
			return fmt.Errorf("Hermes environment variable %q must not be sourced from the cluster", variable.Name)
		}
	}
	return nil
}

// hermesPodManifest is the pod ARIES creates for one attempt. It carries the
// non-secret terminal environment only: the model key and bridge identity are
// staged into the running pod afterwards and never enter the API object.
func hermesPodManifest(active *kubeSession, namespace, image, nodeRole string, environment []string, limits map[string]string) ([]byte, error) {
	env := make([]any, 0, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !validEnvironmentName(name) {
			return nil, fmt.Errorf("Hermes environment entry %q is malformed", name)
		}
		env = append(env, map[string]string{"name": name, "value": value})
	}
	container := map[string]any{
		"name": kubeContainerName, "image": image, "imagePullPolicy": "IfNotPresent",
		"command": kubeIdleCommand, "env": env,
	}
	if len(limits) != 0 {
		// Limits only: the API server copies them into requests, which is what
		// Docker's hard NanoCPUs/Memory amount to.
		container["resources"] = map[string]any{"limits": limits}
	}
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"terminationGracePeriodSeconds": kubeGracePeriodSeconds,
		"containers":                    []any{container},
	}
	// Pin the agent pod to its dedicated pool. Both halves are required: the
	// nodeSelector pulls it onto a labelled node, the toleration admits it past
	// the NoSchedule taint. No role means no pinning, so an unlabelled cluster
	// still schedules the pod.
	if nodeRole != "" {
		spec["nodeSelector"] = map[string]string{nodeRoleLabel: nodeRole}
		spec["tolerations"] = []any{map[string]string{
			"key": nodeRoleLabel, "operator": "Equal", "value": nodeRole, "effect": "NoSchedule",
		}}
	}
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": active.podName, "namespace": namespace,
			"labels": map[string]string{
				"app.kubernetes.io/name": "aries-hermes", "app.kubernetes.io/component": "harness",
				// managed-by is what the resource source selects on; without it
				// the agent pod is invisible to pod telemetry.
				"app.kubernetes.io/managed-by": "aries",
				"aries.dev/attempt":            active.attemptID,
			},
			// Run and task IDs can exceed the 63-byte label limit.
			"annotations": map[string]string{"aries.dev/run-id": active.runID, "aries.dev/task-id": active.taskID},
		},
		"spec": spec,
	}
	return json.Marshal(pod)
}

// kubeResourceLimits converts the task's CPU and memory into pod limits, with
// the same validation the Docker backend applies.
func kubeResourceLimits(request core.HarnessRequest) (map[string]string, error) {
	resources, err := harnessResources(request)
	if err != nil {
		return nil, err
	}
	limits := map[string]string{}
	if resources.NanoCPUs > 0 {
		// Millicores are the finest CPU unit the kubelet honours.
		limits["cpu"] = strconv.FormatInt(max(1, resources.NanoCPUs/1_000_000), 10) + "m"
	}
	if resources.Memory > 0 {
		limits["memory"] = strconv.FormatInt(resources.Memory, 10)
	}
	return limits, nil
}

var (
	// dnsLabelPattern is a namespace name.
	dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	// labelValuePattern is a Kubernetes label value, which a node role must be
	// to appear in a nodeSelector.
	labelValuePattern = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
)

// execKubectl is the production kubectlRunner. Output is bounded, so a runaway
// command cannot exhaust ARIES's memory; exceeding the bound is reported by the
// caller as an error rather than silently truncated.
type execKubectl struct{ path string }

func (runner execKubectl) Run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, runner.path, args...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = maxDockerOutput+1, maxDockerOutput+1
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		// The first arguments name the verb and object, which is enough to
		// diagnose; later ones can include the task instruction.
		verb := strings.Join(args[:min(len(args), 4)], " ")
		return stdout.Bytes(), stderr.Bytes(), fmt.Errorf("kubectl %s: %w: %s", verb, err, lastLine(stderr.Bytes()))
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

func lastLine(content []byte) string {
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	line := lines[len(lines)-1]
	if len(line) > 512 {
		line = line[:512]
	}
	return line
}
