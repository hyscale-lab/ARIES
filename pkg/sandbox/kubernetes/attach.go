package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// AttachOptions name a task pod that another ARIES process created and still
// owns. Every field comes from that owner, never from the harness.
type AttachOptions struct {
	Namespace   string
	PodName     string
	SandboxID   string
	Workdir     string
	RunID       string
	TaskID      string
	KubectlPath string
	Logger      *logrus.Logger
}

// Attach returns a handle on an existing task pod for the tool bridge when it
// runs apart from the runner. The handle can exec into the pod but cannot stop
// it: its owner is a private manager that nothing else can reach, and
// Manager.Stop refuses a sandbox it does not own. The runner keeps the pod's
// lifecycle.
//
// The pod is checked before anything runs in it. The request names the pod,
// so a wrong or malicious request could otherwise point the bridge at the
// harness pod or at another task's sandbox; Attach accepts only a live ARIES
// sandbox pod whose generated ID, run and task all match.
func Attach(ctx context.Context, options AttachOptions) (*Sandbox, error) {
	if err := validateIdentity("run", options.RunID); err != nil {
		return nil, err
	}
	if err := validateIdentity("task", options.TaskID); err != nil {
		return nil, err
	}
	if !validSandboxID(options.SandboxID) {
		return nil, fmt.Errorf("kubernetes sandbox ID %q is not one ARIES generates", options.SandboxID)
	}
	// The name is derived from the ID in Start. Requiring the same derivation
	// here means a request cannot pair a valid ID with some other pod.
	if options.PodName != "aries-task-"+options.SandboxID {
		return nil, fmt.Errorf("kubernetes pod %q does not belong to sandbox %q", options.PodName, options.SandboxID)
	}
	if options.Namespace == "" {
		options.Namespace = defaultNamespace
	}
	if options.Workdir == "" {
		options.Workdir = "/"
	}
	if _, err := validatePath(options.Workdir, false); err != nil {
		return nil, fmt.Errorf("kubernetes sandbox workdir: %w", err)
	}
	if options.KubectlPath == "" {
		options.KubectlPath = defaultKubectl
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	owner := &Manager{
		namespace: options.Namespace, kubectl: options.KubectlPath,
		cleanupTimeout: defaultCleanupTimeout, logger: options.Logger,
	}
	sandbox := &Sandbox{
		owner: owner, namespace: options.Namespace, podName: options.PodName,
		sandboxID: options.SandboxID, workdir: options.Workdir,
		runID: options.RunID, taskID: options.TaskID, cleanupTimeout: defaultCleanupTimeout,
	}
	out, err := owner.run(ctx, "get", "pod", "-n", sandbox.namespace, sandbox.podName, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("read kubernetes task pod: %w", err)
	}
	if err := checkAttachable(out, sandbox); err != nil {
		return nil, err
	}
	return sandbox, nil
}

type attachedPod struct {
	Metadata struct {
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func checkAttachable(raw []byte, sandbox *Sandbox) error {
	var pod attachedPod
	if err := json.Unmarshal(raw, &pod); err != nil {
		return fmt.Errorf("parse kubernetes task pod: %w", err)
	}
	labels, annotations := pod.Metadata.Labels, pod.Metadata.Annotations
	var problems []string
	if labels["app.kubernetes.io/managed-by"] != "aries" || labels["app.kubernetes.io/component"] != "sandbox" {
		problems = append(problems, "it is not an ARIES sandbox pod")
	}
	if labels[sandboxIDLabel] != sandbox.sandboxID {
		problems = append(problems, fmt.Sprintf("its sandbox ID is %q", labels[sandboxIDLabel]))
	}
	if annotations["aries.dev/run-id"] != sandbox.runID || annotations["aries.dev/task-id"] != sandbox.taskID {
		problems = append(problems, "it belongs to a different run or task")
	}
	if pod.Metadata.DeletionTimestamp != nil {
		problems = append(problems, "it is being deleted")
	}
	if pod.Status.Phase != "Running" {
		problems = append(problems, fmt.Sprintf("its phase is %q", pod.Status.Phase))
	}
	if len(problems) > 0 {
		return fmt.Errorf("refusing to attach to kubernetes pod %s/%s: %s", sandbox.namespace, sandbox.podName, strings.Join(problems, "; "))
	}
	return nil
}

// validSandboxID accepts exactly what randomID produces: 16 lowercase hex
// characters.
func validSandboxID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Namespace is the namespace the task pod runs in.
func (s *Sandbox) Namespace() string { return s.namespace }

// SandboxID is the generated ID the task pod and its NetworkPolicy carry.
func (s *Sandbox) SandboxID() string { return s.sandboxID }
