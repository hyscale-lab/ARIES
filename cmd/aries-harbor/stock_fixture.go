package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func exportStockFixture(r *harbor.Recording, w io.Writer) error {
	switch r.Task {
	case "wal-recovery-ordering", "session-window-debug", "cargo-flight-dispatch":
	default:
		return fmt.Errorf("stock fixture preparation unavailable for %s", r.Task)
	}
	task, ok := sourceSetup(r)
	_, image, qualified := strings.Cut(r.Image, "@")
	if !ok || !qualified || !strings.HasPrefix(image, "sha256:") {
		return fmt.Errorf("missing reviewed fixture for %s", r.Task)
	}
	check := "set -eu\n" + task.initialCheck + "\n" + task.inputCheck +
		"\ncommand -v node codex rg python3\ntest \"$(node --version)\" = v22.23.2\nrg --version | head -1"
	return json.NewEncoder(w).Encode(map[string]any{
		"task": r.Task, "trial_id": r.Trial, "workdir": r.Workdir, "cpus": r.VCPUs, "memory_mib": r.MemoryMiB,
		"image_digest": image, "trajectory_sha256": r.TrajectoryHash, "rollout_sha256": r.RolloutHash,
		"codex_prefix_sha256": codexPrefixSHA, "packages_sha256": task.packagesSHA, "output": task.output, "ready_script": check,
	})
}
