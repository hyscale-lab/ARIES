package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	componentLabel = "app.kubernetes.io/component"
	// runtimeLabel carries the pod's own random name. Request labels may exceed
	// the 63-byte label-value limit, so they are stored as annotations and this
	// label is what a NetworkPolicy selects exactly one pod with.
	runtimeLabel = "aries.dev/runtime"
)

var (
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
	labelValue   = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	// labelKey is an optional DNS-subdomain prefix and a name of up to 63 characters.
	labelKey = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
)

func validName(value string) bool      { return dnsLabel.MatchString(value) }
func validSubdomain(value string) bool { return dnsSubdomain.MatchString(value) }

// validateNodeSelector keeps the configured labels within the label grammar, so
// an invalid profile fails at construction rather than at the first pod.
func validateNodeSelector(selector map[string]string) error {
	for key, value := range selector {
		if !labelKey.MatchString(key) || len(key) > 317 {
			return fmt.Errorf("Kubernetes node selector key %q is not a label key", key)
		}
		if value != "" && !labelValue.MatchString(value) {
			return fmt.Errorf("Kubernetes node selector value %q is not a label value", value)
		}
	}
	return nil
}

func validateNumericUser(value string) error {
	uid, gid, found := strings.Cut(value, ":")
	if !found || !decimalDigits(uid) || !decimalDigits(gid) {
		return errors.New("user must be a numeric UID:GID pair")
	}
	return nil
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func cleanContainerPath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) || !strings.HasPrefix(value, "/") {
		return "", errors.New("path must be absolute, nonempty, and NUL-free")
	}
	if path.Clean(value) != value {
		return "", errors.New("path must be clean")
	}
	if value == "/" {
		return "", errors.New("path must not be the container root")
	}
	return value, nil
}

// validateRequest rejects settings this provider cannot honor instead of
// silently dropping them.
func (manager *Manager) validateRequest(request deployment.Request) error {
	if request.HostPID {
		return errors.New("Kubernetes deployment does not support host PID access")
	}
	if !validName(request.Name) {
		return fmt.Errorf("Kubernetes deployment requires a DNS-label runtime name, got %q", request.Name)
	}
	if request.Placement.AttachmentID != manager.options.Namespace {
		return fmt.Errorf("Kubernetes deployment requires placement in namespace %q", manager.options.Namespace)
	}
	if strings.TrimSpace(request.Image) == "" {
		return errors.New("Kubernetes deployment requires an image")
	}
	if len(request.Mounts) != 0 {
		return errors.New("Kubernetes deployment does not mount host paths")
	}
	if len(request.NetworkAliases) != 0 {
		return errors.New("Kubernetes deployment does not support network aliases")
	}
	for _, port := range []int{request.InternalPort, request.ServicePort} {
		if port < 0 || port > 65535 {
			return errors.New("deployment service port is invalid")
		}
	}
	if request.CPU != nil && (*request.CPU <= 0 || math.IsNaN(*request.CPU) || math.IsInf(*request.CPU, 0)) {
		return errors.New("deployment CPU must be finite and positive")
	}
	if request.MemoryMB != nil && *request.MemoryMB <= 0 {
		return errors.New("deployment memory must be positive")
	}
	if request.GPUs < 0 || request.StorageMB < 0 {
		return errors.New("deployment GPU and storage bounds must be nonnegative")
	}
	for _, entry := range request.Env {
		if name, _, found := strings.Cut(entry, "="); !found || name == "" {
			return fmt.Errorf("deployment environment entry %q is not NAME=value", entry)
		}
	}
	_, err := gatedCommand(request)
	return err
}

// gatedCommand wraps the requested command behind the start gate. Kubernetes
// cannot report an image's entrypoint, so a request that leaves Entrypoint
// unset runs its Args directly; such images must not depend on their entrypoint.
func gatedCommand(request deployment.Request) ([]string, error) {
	command := append(append([]string{}, request.Entrypoint...), request.Args...)
	if len(command) == 0 {
		return nil, errors.New("Kubernetes deployment requires an explicit command")
	}
	for _, argument := range command {
		if strings.ContainsRune(argument, 0) {
			return nil, errors.New("deployment command contains NUL")
		}
	}
	return append([]string{"/bin/sh", "-c", gateShell, "aries-gate"}, command...), nil
}

func (manager *Manager) podManifest(request deployment.Request) ([]byte, error) {
	command, err := gatedCommand(request)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{managedByLabel: "aries", runtimeLabel: request.Name}
	if component := request.Labels["aries.component"]; labelValue.MatchString(component) {
		labels[componentLabel] = component
	}
	mounts := []any{map[string]any{"name": "aries-gate", "mountPath": gateDirectory}}
	volumes := []any{map[string]any{"name": "aries-gate", "emptyDir": map[string]any{}}}
	var stagingMounts []any
	for index, directory := range manager.options.StagingDirs {
		name := "aries-staging-" + strconv.Itoa(index)
		mount := map[string]any{"name": name, "mountPath": directory}
		mounts = append(mounts, mount)
		stagingMounts = append(stagingMounts, mount)
		volumes = append(volumes, map[string]any{"name": name, "emptyDir": map[string]any{}})
	}
	container := map[string]any{
		"name": containerName, "image": manager.image(request.Image), "command": command,
		"volumeMounts": mounts,
	}
	if request.Workdir != "" {
		container["workingDir"] = request.Workdir
	}
	if len(request.Env) != 0 {
		env := make([]any, 0, len(request.Env))
		for _, entry := range request.Env {
			name, value, _ := strings.Cut(entry, "=")
			env = append(env, map[string]string{"name": name, "value": value})
		}
		container["env"] = env
	}
	var ports []any
	for _, port := range []int{request.ServicePort, request.InternalPort} {
		if port != 0 {
			ports = append(ports, map[string]any{"containerPort": port, "protocol": "TCP"})
		}
	}
	if len(ports) != 0 {
		container["ports"] = ports
	}
	if limits := resourceLimits(request); len(limits) != 0 {
		container["resources"] = map[string]any{"limits": limits, "requests": limits}
	}
	if request.NoNewPrivileges {
		container["securityContext"] = map[string]any{"allowPrivilegeEscalation": false}
	}
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"automountServiceAccountToken":  manager.options.ServiceAccount != "",
		"enableServiceLinks":            false,
		"terminationGracePeriodSeconds": 5,
		"containers":                    []any{container},
		"volumes":                       volumes,
	}
	if manager.options.ServiceAccount != "" {
		spec["serviceAccountName"] = manager.options.ServiceAccount
	}
	if manager.options.ImagePullSecret != "" {
		spec["imagePullSecrets"] = []any{map[string]string{"name": manager.options.ImagePullSecret}}
	}
	if len(stagingMounts) != 0 {
		// The image's own user cannot create directories under root-owned
		// paths, so a root init container hands the staging roots to it first.
		spec["initContainers"] = []any{map[string]any{
			"name": "aries-staging", "image": manager.image(request.Image),
			"command":         append([]string{"/bin/sh", "-c", `owner=$1; shift; chown "$owner" "$@"`, "aries-staging", manager.options.StagingOwner}, manager.options.StagingDirs...),
			"securityContext": map[string]any{"runAsUser": 0},
			"volumeMounts":    stagingMounts,
		}}
	}
	if len(manager.options.NodeSelector) != 0 {
		spec["nodeSelector"] = manager.options.NodeSelector
	}
	metadata := map[string]any{"name": request.Name, "namespace": manager.options.Namespace, "labels": labels}
	if len(request.Labels) != 0 {
		metadata["annotations"] = request.Labels
	}
	return json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": metadata, "spec": spec})
}

func resourceLimits(request deployment.Request) map[string]string {
	limits := map[string]string{}
	if request.CPU != nil {
		limits["cpu"] = strconv.FormatFloat(*request.CPU, 'f', -1, 64)
	}
	if request.MemoryMB != nil {
		limits["memory"] = strconv.Itoa(*request.MemoryMB) + "Mi"
	}
	if request.StorageMB > 0 {
		limits["ephemeral-storage"] = strconv.Itoa(request.StorageMB) + "Mi"
	}
	if request.GPUs > 0 {
		limits["nvidia.com/gpu"] = strconv.Itoa(request.GPUs)
	}
	return limits
}

// podObject is the subset of a pod this provider reads back.
type podObject struct {
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName                     string `json:"nodeName"`
		HostNetwork                  bool   `json:"hostNetwork"`
		HostPID                      bool   `json:"hostPID"`
		HostIPC                      bool   `json:"hostIPC"`
		AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken"`
		Containers                   []struct {
			Image           string   `json:"image"`
			Command         []string `json:"command"`
			Args            []string `json:"args"`
			WorkingDir      string   `json:"workingDir"`
			SecurityContext *struct {
				AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation"`
				Privileged               *bool `json:"privileged"`
			} `json:"securityContext"`
			Resources struct {
				Limits map[string]string `json:"limits"`
			} `json:"resources"`
		} `json:"containers"`
		Volumes []struct {
			HostPath *struct{} `json:"hostPath"`
		} `json:"volumes"`
	} `json:"spec"`
	Status struct {
		Phase                 string            `json:"phase"`
		PodIP                 string            `json:"podIP"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type containerStatus struct {
	State struct {
		Waiting *struct {
			Reason string `json:"reason"`
		} `json:"waiting"`
	} `json:"state"`
}
