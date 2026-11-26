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
	case "wal-recovery-ordering", "session-window-debug", "cargo-flight-dispatch", "mvcc-lsm-compaction",
		"freight-dispatch-shift", "embedding-drift-monitor", bipedTask, vppLossTask, soundCascadeTask, "coq-block-bound", pretrainShardTask,
		"freecad-platform-drawing", "freecad-spring-clip", "music-harmony":
	default:
		return fmt.Errorf("stock fixture preparation unavailable for %s", r.Task)
	}
	task, ok := sourceSetup(r)
	_, image, qualified := strings.Cut(r.Image, "@")
	if !ok || !qualified || !strings.HasPrefix(image, "sha256:") {
		return fmt.Errorf("missing reviewed fixture for %s", r.Task)
	}
	final, err := taskOutputChecks(r, false)
	if err != nil {
		return err
	}
	node := "node"
	if r.Task == "freecad-platform-drawing" || r.Task == "freecad-spring-clip" {
		node = "/root/.nvm/versions/node/v22.23.2/bin/node"
	}
	imageHelper := ""
	for _, call := range r.Plan.Calls {
		if call.Operation == "ViewImage" {
			imageHelper = "470c750e1af208d646f53c77a5334c33224a107693e2d02e83df124e50e7cde5"
		}
	}
	check := "set -eu\n" + task.initialCheck + "\n" + task.inputCheck +
		"\ncommand -v node codex rg python3\ntest \"$(" + node + " --version)\" = v22.23.2\nrg --version | head -1"
	return json.NewEncoder(w).Encode(map[string]any{
		"task": r.Task, "trial_id": r.Trial, "workdir": r.Workdir, "cpus": r.VCPUs, "memory_mib": r.MemoryMiB,
		"image_digest": image, "trajectory_sha256": r.TrajectoryHash, "rollout_sha256": r.RolloutHash,
		"codex_prefix_sha256": codexPrefixSHA, "packages_sha256": task.packagesSHA, "output": task.output, "ready_script": check, "final_script": final, "image_helper_sha256": imageHelper,
	})
}
