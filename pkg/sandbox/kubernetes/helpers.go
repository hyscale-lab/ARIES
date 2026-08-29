package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
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
			},
			// Run and task IDs can exceed the 63-byte label limit, so they are
			// annotations (no length cap) rather than labels.
			"annotations": map[string]string{
				"aries.dev/run-id":  request.RunID,
				"aries.dev/task-id": request.TaskID,
			},
		},
		"spec": map[string]any{
			"restartPolicy":                 "Never",
			"automountServiceAccountToken":  false,
			"terminationGracePeriodSeconds": 5,
			"containers":                    []any{container},
		},
	}
	return json.Marshal(pod)
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

// resourceSource is a placeholder ResourceSource. Pod-level CPU/memory metrics
// require the Kubernetes metrics API (metrics-server) or cAdvisor; wiring that
// is deferred, so this reports no readings without failing a run.
type resourceSource struct{}

// NewResourceSource returns a no-op resource source for the Kubernetes backend.
func NewResourceSource() monitor.ResourceSource { return resourceSource{} }

func (resourceSource) Sample(context.Context) ([]core.ResourceReading, error) { return nil, nil }
func (resourceSource) Close() error                                           { return nil }
