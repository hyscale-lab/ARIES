package kubernetes

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	arsandbox "github.com/hyscale-lab/aries/pkg/sandbox"
)

const maxBridgeFileSize = 64 << 20

// validateIdentity mirrors the Docker backend's run/task identity rules.
func validateIdentity(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("kubernetes sandbox %s identity is required", kind)
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return fmt.Errorf("kubernetes sandbox %s identity %q has invalid characters", kind, value)
		}
	}
	return nil
}

func validateCommand(command core.Command) error {
	if strings.TrimSpace(command.Path) == "" {
		return fmt.Errorf("kubernetes command requires a path")
	}
	if strings.ContainsRune(command.Path, 0) {
		return fmt.Errorf("kubernetes command path contains NUL")
	}
	for _, arg := range command.Args {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("kubernetes command argument contains NUL")
		}
	}
	return nil
}

// validatePath enforces the same absolute/normalized/NUL-free contract the
// bridge expects, rejecting the container root for mutating operations.
func validatePath(value string, rejectRoot bool) (string, error) {
	if value == "" || strings.ContainsRune(value, 0) || !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("path must be absolute, nonempty, and NUL-free")
	}
	clean := path.Clean(value)
	if clean != value {
		return "", fmt.Errorf("path must be normalized")
	}
	if rejectRoot && clean == "/" {
		return "", fmt.Errorf("container root cannot be modified or removed")
	}
	return clean, nil
}

// shellQuote wraps a value in single quotes for safe use in an sh -c string.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// shellCommand renders a core.Command's path + args as a quoted sh fragment.
func shellCommand(command core.Command) string {
	parts := make([]string, 0, len(command.Args)+1)
	parts = append(parts, shellQuote(command.Path))
	for _, arg := range command.Args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// sortedEnv renders an env map as deterministic K=V argv tokens.
func sortedEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

// parseStat converts "size|modehex|mtime|humantype" into a FileInfo.
func parseStat(fullPath, name, fields string) (arsandbox.FileInfo, error) {
	parts := strings.SplitN(fields, "|", 4)
	if len(parts) != 4 {
		return arsandbox.FileInfo{}, fmt.Errorf("unexpected stat output %q", fields)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		return arsandbox.FileInfo{}, fmt.Errorf("parse stat size: %w", err)
	}
	modeBits, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 16, 32)
	if err != nil {
		return arsandbox.FileInfo{}, fmt.Errorf("parse stat mode: %w", err)
	}
	mtime, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
	if err != nil {
		return arsandbox.FileInfo{}, fmt.Errorf("parse stat mtime: %w", err)
	}
	return arsandbox.FileInfo{
		Name:    name,
		Path:    fullPath,
		Type:    entryType(strings.TrimSpace(parts[3])),
		Size:    size,
		Mode:    os.FileMode(modeBits & uint64(os.ModePerm)),
		ModTime: time.Unix(mtime, 0).UTC(),
	}, nil
}

func entryType(humanType string) string {
	switch humanType {
	case "directory":
		return "directory"
	case "symbolic link":
		return "symlink"
	case "regular file", "regular empty file":
		return "file"
	default:
		return "other"
	}
}

// podManifest renders the task pod as JSON for `kubectl apply`.
func podManifest(sandbox *Sandbox, request core.SandboxRequest) ([]byte, error) {
	env := make([]map[string]string, 0, len(request.Environment.Env))
	for _, kv := range sortedEnv(request.Environment.Env) {
		key, value, _ := strings.Cut(kv, "=")
		env = append(env, map[string]string{"name": key, "value": value})
	}
	container := map[string]any{
		"name":       "sandbox",
		"image":      request.Environment.Image,
		"command":    []string{"/bin/sh", "-c", "sleep infinity"},
		"workingDir": sandbox.workdir,
	}
	if len(env) > 0 {
		container["env"] = env
	}
	if resources := containerResources(request.Environment); resources != nil {
		container["resources"] = resources
	}
	pod := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      sandbox.podName,
			"namespace": sandbox.namespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "aries",
				"app.kubernetes.io/component":  "sandbox",
				// The generated sandbox ID is short and label-safe by
				// construction, unlike the run/task IDs below. It gives the
				// per-task NetworkPolicy a selector that matches this pod and
				// no other, which is what makes isolation per-task rather than
				// namespace-wide.
				sandboxIDLabel: sandbox.sandboxID,
			},
			// Run and task IDs can exceed the 63-byte label limit, so they are
			// annotations (no length cap) rather than labels.
			"annotations": map[string]string{
				"aries.dev/run-id":  request.RunID,
				"aries.dev/task-id": request.TaskID,
			},
		},
		"spec": podSpec(container, sandbox.owner.nodeRole),
	}
	return json.Marshal(pod)
}

// podSpec builds the task pod's spec, pinning it to a dedicated node pool when
// a role is configured. Both halves are required: the nodeSelector pulls the
// pod onto a labelled node, and the toleration gets it past the NoSchedule
// taint that keeps everything else off. An empty role omits both, so a cluster
// whose nodes carry no role labels still schedules task pods normally.
func podSpec(container map[string]any, nodeRole string) map[string]any {
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"automountServiceAccountToken":  false,
		"terminationGracePeriodSeconds": 5,
		"containers":                    []any{container},
	}
	if nodeRole == "" {
		return spec
	}
	spec["nodeSelector"] = map[string]string{nodeRoleLabel: nodeRole}
	spec["tolerations"] = []any{map[string]any{
		"key":      nodeRoleLabel,
		"operator": "Equal",
		"value":    nodeRole,
		"effect":   "NoSchedule",
	}}
	return spec
}

// networkPolicyManifest builds the NetworkPolicy that isolates one task pod.
// Every task gets one; `allowNetwork` changes what it permits, never whether it
// exists.
//
// This mirrors the Docker backend, which gives each task its *own* network and
// sets `Internal` on it. Those are two separate guarantees, and conflating them
// is easy: `Internal` controls whether the task reaches the internet, while the
// per-task network is what stops two concurrent tasks reaching each other. A
// Kubernetes cluster has one flat pod network, so both must be expressed here.
//
//	allowNetwork=false → no ingress, no egress. The air-gapped equivalent of
//	                     Docker's `Internal: true`.
//	allowNetwork=true  → no ingress; egress to the internet but not to the
//	                     cluster's own pod and service networks.
//
// Ingress is denied in both cases, and that is the load-bearing half: a task pod
// that accepts no connections cannot be reached by another task regardless of
// what that other task is permitted to send. Inter-task isolation therefore does
// not depend on the CIDRs below being configured correctly — misconfiguring them
// loosens what a task can dial out to, but never lets tasks talk to each other.
//
// The deny spelling is subtle. Naming a direction in policyTypes with *no*
// corresponding rule block denies it; adding an empty rule list would instead
// permit everything. So for the air-gapped case the absence of the "egress" key
// is what does the work.
//
// IMPORTANT: NetworkPolicy is enforced by the CNI plugin, not by Kubernetes.
// Under a plugin that does not implement it (flannel, for one) the API server
// still accepts this object and silently enforces nothing. See
// k8s/install/README.md for the CNI requirement.
func networkPolicyManifest(sandbox *Sandbox, allowNetwork bool) ([]byte, error) {
	spec := map[string]any{
		"podSelector": map[string]any{
			"matchLabels": map[string]string{sandboxIDLabel: sandbox.sandboxID},
		},
		"policyTypes": []any{"Ingress", "Egress"},
	}
	if allowNetwork {
		spec["egress"] = egressRules(sandbox.owner.podCIDR, sandbox.owner.serviceCIDR)
	}
	policy := map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name":      sandbox.policyName,
			"namespace": sandbox.namespace,
			"labels": map[string]string{
				"app.kubernetes.io/managed-by": "aries",
				"app.kubernetes.io/component":  "sandbox",
				sandboxIDLabel:                 sandbox.sandboxID,
			},
			"annotations": map[string]string{
				"aries.dev/run-id":  sandbox.runID,
				"aries.dev/task-id": sandbox.taskID,
			},
		},
		"spec": spec,
	}
	return json.Marshal(policy)
}

// egressRules permits a network-allowed task to reach the internet while keeping
// it off the cluster's own networks — other task pods, the agent pods, the ARIES
// pod, and the API server.
//
// DNS is allowed by selecting the CoreDNS pods rather than the kube-dns Service
// IP. Service IPs are DNAT'd by kube-proxy *before* egress policy is evaluated,
// so a rule naming the Service address would never match; the destination the
// policy actually sees is a CoreDNS pod IP, which the cluster-CIDR exclusion
// below would otherwise drop. Getting this wrong breaks name resolution in every
// network-allowed task while leaving raw IP traffic working — a confusing
// failure, hence the explicit carve-out.
//
// With no CIDRs configured the cluster exclusion cannot be expressed, so egress
// is left open. Ingress is still denied, so tasks remain isolated from one
// another; only outbound reach is broader than ideal.
func egressRules(podCIDR, serviceCIDR string) []any {
	dns := map[string]any{
		"to": []any{map[string]any{
			"namespaceSelector": map[string]any{
				"matchLabels": map[string]string{"kubernetes.io/metadata.name": "kube-system"},
			},
			"podSelector": map[string]any{
				"matchLabels": map[string]string{"k8s-app": "kube-dns"},
			},
		}},
		"ports": []any{
			map[string]any{"protocol": "UDP", "port": 53},
			map[string]any{"protocol": "TCP", "port": 53},
		},
	}
	except := make([]any, 0, 2)
	for _, cidr := range []string{podCIDR, serviceCIDR} {
		if strings.TrimSpace(cidr) != "" {
			except = append(except, cidr)
		}
	}
	block := map[string]any{"cidr": "0.0.0.0/0"}
	if len(except) > 0 {
		block["except"] = except
	}
	return []any{dns, map[string]any{
		"to": []any{map[string]any{"ipBlock": block}},
	}}
}

// validateNodeRole keeps the configured role within the Kubernetes label-value
// grammar, so an invalid profile fails at construction rather than producing a
// pod the API server rejects at task start.
func validateNodeRole(role string) error {
	if role == "" {
		return nil
	}
	if len(role) > 63 {
		return fmt.Errorf("sandbox node role %q exceeds the 63-character label-value limit", role)
	}
	for _, r := range role {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("sandbox node role %q contains %q; label values allow only alphanumerics, '-', '_' and '.'", role, r)
		}
	}
	if strings.HasPrefix(role, "-") || strings.HasPrefix(role, "_") || strings.HasPrefix(role, ".") ||
		strings.HasSuffix(role, "-") || strings.HasSuffix(role, "_") || strings.HasSuffix(role, ".") {
		return fmt.Errorf("sandbox node role %q must start and end with an alphanumeric", role)
	}
	return nil
}

func containerResources(env core.Environment) map[string]any {
	limits := map[string]string{}
	if env.CPU > 0 {
		limits["cpu"] = strconv.FormatFloat(env.CPU, 'f', -1, 64)
	}
	if env.MemoryMB > 0 {
		limits["memory"] = strconv.Itoa(env.MemoryMB) + "Mi"
	}
	if len(limits) == 0 {
		return nil
	}
	return map[string]any{"limits": limits, "requests": limits}
}
