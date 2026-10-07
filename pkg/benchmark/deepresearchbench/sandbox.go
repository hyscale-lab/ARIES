package deepresearchbench

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// searxngStartScript launches the SearXNG instance built into the DRB task
// image (see images/deep-research-bench/Dockerfile) as a background process.
// ARIES always overrides the task image's own entrypoint with
// `/bin/sleep infinity` (pkg/sandbox/sandbox.go), so nothing in the
// image itself can autostart SearXNG; this is the only place it gets
// launched. The backgrounded python process is reparented to the container's
// PID 1 once this shell exits, which is standard POSIX orphan handling and
// needs no `disown` (not a builtin in /bin/sh's dash on Ubuntu).
const searxngStartScript = "cd /opt/searxng-src && SEARXNG_SETTINGS_PATH=/etc/searxng/settings.yml " +
	"nohup /opt/searxng-venv/bin/python -m searx.webapp >/var/log/searxng.log 2>&1 &"

// searxngHealthCheckURL is probed from inside the sandbox container itself
// (loopback), independently of the fixed `task-sandbox` network alias the
// OpenClaw harness container uses to reach this same instance externally.
const searxngHealthCheckURL = "http://127.0.0.1:8888/search?format=json&q=aries-healthcheck"

// Package-level vars, not consts, so tests can shrink them to avoid waiting
// out the real bound.
var (
	searxngHealthCheckAttempts = 40
	searxngHealthCheckDelay    = 500 * time.Millisecond
)

// PrepareSandbox confirms the agent's designated report path starts absent
// before the harness gets bridge access, then starts the sandbox's local
// SearXNG instance and waits for it to accept requests. Deep Research Bench
// has no sandbox-resident private verifier tree like Terminal-Bench 2's
// /tests: its private material (the reference report and RACE rubric) never
// enters the sandbox at all, staying host-side for the judge call in
// Evaluate. This step instead guards the one channel Evaluate trusts, the
// report path, against a reused or dirty sandbox pre-seeding a fake report.
func (b *Benchmark) PrepareSandbox(ctx context.Context, task core.Task, sandbox runner.Sandbox) error {
	if sandbox == nil {
		return errors.New("deepresearchbench preparation requires a live sandbox")
	}
	removed, err := sandbox.Exec(ctx, core.Command{Path: "/bin/rm", Args: []string{"-rf", "--", reportPath}})
	if err != nil {
		return fmt.Errorf("remove report path before harness: %w", err)
	}
	if removed.ExitCode != 0 {
		return fmt.Errorf("remove report path before harness: exit code %d", removed.ExitCode)
	}
	const absencePredicate = `for path do [ ! -e "$path" ] && [ ! -L "$path" ] || exit 1; done`
	probed, err := sandbox.Exec(ctx, core.Command{
		Path: "/bin/sh", Args: []string{"-c", absencePredicate, "aries-report-absence", reportPath},
	})
	if err != nil {
		return fmt.Errorf("confirm report path absent before harness: %w", err)
	}
	if probed.ExitCode != 0 {
		return fmt.Errorf("confirm report path absent before harness: exit code %d", probed.ExitCode)
	}
	if err := startSearXNG(ctx, sandbox); err != nil {
		return err
	}
	return nil
}

func startSearXNG(ctx context.Context, sandbox runner.Sandbox) error {
	started, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", searxngStartScript}})
	if err != nil {
		return fmt.Errorf("start SearXNG: %w", err)
	}
	if started.ExitCode != 0 {
		return fmt.Errorf("start SearXNG: exit code %d", started.ExitCode)
	}
	for attempt := 0; attempt < searxngHealthCheckAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("await SearXNG readiness: %w", ctx.Err())
			case <-time.After(searxngHealthCheckDelay):
			}
		}
		probed, err := sandbox.Exec(ctx, core.Command{
			Path: "/bin/sh", Args: []string{"-c", "curl -sf -o /dev/null " + searxngHealthCheckURL},
		})
		if err == nil && probed.ExitCode == 0 {
			return nil
		}
	}
	return errors.New("SearXNG did not become ready before the bound")
}
