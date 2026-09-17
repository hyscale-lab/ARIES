package toolathlon

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// The grader runs inside the sandbox the agent had root in, after the
// harness is gone. Its code is re-installed from the host at evaluation, so
// what is left to trust is the Python runtime the pinned checkout does not
// carry: uv, the interpreter uv manages, the project's virtualenv, and the
// files uv and Python read before any project code runs. Those are
// inventoried file by file before the bridge exists and again before the
// grader runs; any difference refuses the evaluation. Bytecode caches are
// left out of the inventory because the grader never loads them: it runs
// with a fresh cache prefix and without the user site directory, so a
// planted .pyc or a user-site .pth is inert. The image's system programs
// (the shell, tar, find, sha256sum, libc) are outside this inventory: an
// agent that replaces those is beyond what a same-container grader can
// defend against, the limit ARIES's other benchmarks share.

const (
	runtimeManifestContainerPath = privateRoot + "/runtime-manifest.txt"
	runtimeManifestHostName      = "runtime-manifest.txt"
	pycachePrefixPath            = privateRoot + "/pycache"
)

var runtimeManifestTimeout = 10 * time.Minute

// inventoryProgram is the inventory itself (inventory.py): one SHA-256 per
// regular file under the virtualenv, the interpreter's home, the uv binary
// and the files uv reads for its configuration, every symbolic link under
// the first two with its target, and every regular file at the top of the
// project directory, where Python would find a planted sitecustomize;
// bytecode caches and .log files skipped (see above and inventory.py). It runs on the image's system
// Python, never on the virtualenv it inventories, and releases every file
// it read from the page cache afterwards: ARIES's monitor reports the
// sandbox cgroup's usage, cache included, and a 1.5 GB read would otherwise
// raise that figure by 1.5 GB for the rest of the run.
//
//go:embed inventory.py
var inventoryProgram []byte

// writeRuntimeManifest inventories the evaluator runtime inside the sandbox
// and keeps the inventory at hostPath.
func writeRuntimeManifest(ctx context.Context, sandbox runner.Sandbox, hostPath string) error {
	result, err := sandbox.Exec(ctx, core.Command{
		Path:    "/bin/sh",
		Args:    []string{"-c", `exec python3 - "$@"`, "aries-toolathlon-runtime", workspaceRoot, runtimeManifestContainerPath},
		Stdin:   inventoryProgram,
		Timeout: runtimeManifestTimeout,
	})
	if err != nil {
		return fmt.Errorf("inventory evaluator runtime: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("inventory evaluator runtime: exit code %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	if err := sandbox.Download(ctx, runtimeManifestContainerPath, hostPath); err != nil {
		return fmt.Errorf("download evaluator runtime inventory: %w", err)
	}
	if err := removePaths(ctx, sandbox, []string{runtimeManifestContainerPath}); err != nil {
		return err
	}
	entries, err := parseRuntimeManifest(hostPath)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("evaluator runtime inventory is empty")
	}
	return nil
}

// parseRuntimeManifest maps each inventoried path (or symlink) to its digest
// (or target). A line is "<sha256>  <path>" or "link <path> -> <target>".
func parseRuntimeManifest(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read evaluator runtime inventory: %w", err)
	}
	defer file.Close()
	entries := make(map[string]string)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "link "); ok {
			name, target, found := strings.Cut(rest, " -> ")
			if !found {
				return nil, fmt.Errorf("malformed inventory line %q", line)
			}
			entries["link "+name] = target
			continue
		}
		digest, name, found := strings.Cut(line, "  ")
		if !found || len(digest) != 64 {
			return nil, fmt.Errorf("malformed inventory line %q", line)
		}
		entries[name] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read evaluator runtime inventory: %w", err)
	}
	return entries, nil
}

// compareRuntimeManifests reports every path whose presence or digest differs
// between the inventory taken before the harness existed and the one taken
// before the grader runs.
func compareRuntimeManifests(beforePath, afterPath string) error {
	before, err := parseRuntimeManifest(beforePath)
	if err != nil {
		return err
	}
	after, err := parseRuntimeManifest(afterPath)
	if err != nil {
		return err
	}
	var changed, added, removed []string
	for name, digest := range before {
		if got, ok := after[name]; !ok {
			removed = append(removed, name)
		} else if got != digest {
			changed = append(changed, name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			added = append(added, name)
		}
	}
	if len(changed)+len(added)+len(removed) == 0 {
		return nil
	}
	sort.Strings(changed)
	sort.Strings(added)
	sort.Strings(removed)
	describe := func(what string, names []string) string {
		if len(names) == 0 {
			return ""
		}
		shown := names
		if len(shown) > 5 {
			shown = shown[:5]
		}
		return fmt.Sprintf(" %s %d (%s)", what, len(names), strings.Join(shown, ", "))
	}
	return fmt.Errorf("evaluator runtime changed while the agent ran:%s%s%s", describe("changed", changed), describe("added", added), describe("removed", removed))
}
