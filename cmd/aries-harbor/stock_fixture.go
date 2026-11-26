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
	case nextjsTask:
		if r.Trial != nextjsWave3Trial {
			return fmt.Errorf("stock Next.js fixture requires trial %s", nextjsWave3Trial)
		}
	case paymentsTask:
		if r.Trial != paymentsLauncherTrial {
			return fmt.Errorf("stock payments fixture requires trial %s", paymentsLauncherTrial)
		}
	case hofTopologyTask:
		if r.Trial != hofReleaseTrial {
			return fmt.Errorf("stock HOF network fixture requires trial %s", hofReleaseTrial)
		}
	case claimsTask, "intrastat-meldung", "wal-recovery-ordering", "session-window-debug", "cargo-flight-dispatch", "mvcc-lsm-compaction",
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
	if r.Task == "freecad-platform-drawing" || r.Task == "freecad-spring-clip" || r.Task == paymentsTask {
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
	meta := map[string]any{
		"task": r.Task, "trial_id": r.Trial, "workdir": r.Workdir, "cpus": r.VCPUs, "memory_mib": r.MemoryMiB,
		"image_digest": image, "trajectory_sha256": r.TrajectoryHash, "rollout_sha256": r.RolloutHash,
		"codex_prefix_sha256": codexPrefixSHA, "packages_sha256": task.packagesSHA, "output": task.output, "ready_script": check, "final_script": final, "image_helper_sha256": imageHelper,
	}
	if r.Task == paymentsTask {
		meta["payments_health"] = paymentsNetworkProbe
		meta["final_script"] = final + "\nfor file in /tmp/worker-integration-{results,final}.log; do echo \"$file\"; if test -f \"$file\"; then grep -E '^(PASS|seeded|respawn|overlap|AssertionError|Traceback)' \"$file\" || true; fi; done\n"
	}
	if r.Task == nextjsTask {
		meta["companions"] = []map[string]any{{"Role": "warehouse", "Image": "localhost/zb-nextjs-warehouse@sha256:decefdfc46bbed17ebdc58a58d0b220ae52d4c71b363ce42b58ecde17d309ba8", "Command": []string{}}}
		meta["nextjs_health"] = nextjsHealthCheck
	}
	if r.Task == "intrastat-meldung" {
		meta["companions"] = intrastatCompanions()
		meta["intrastat_health"] = intrastatHealth
	}
	if r.Task == claimsTask {
		meta["companions"] = claimsCompanions
		meta["claims_health"] = claimsHealth
	}
	return json.NewEncoder(w).Encode(meta)
}
