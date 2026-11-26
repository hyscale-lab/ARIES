package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func runTTCCalendar(ctx context.Context, trace, command, image, arm, tests string, out io.Writer) (runErr error) {
	ctx, cancelRun := context.WithTimeout(ctx, 4*time.Minute)
	defer cancelRun()
	raw, err := os.ReadFile(trace)
	if err != nil {
		return err
	}
	recording, err := harbor.LoadWildClawCalendar(raw)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	mark := func(event string, detail any) error {
		at, err := harbor.Clock()
		if err != nil {
			return err
		}
		return encoder.Encode(struct {
			Event  string `json:"event"`
			Time   int64  `json:"monotonic_ns"`
			Detail any    `json:"detail,omitempty"`
		}{event, at, detail})
	}
	family := fmt.Sprintf("aries-calendar-%d-%d", os.Getpid(), time.Now().UnixNano())
	branch := family + "@main"
	call := func(ctx context.Context, argv ...string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, command, argv...)
		cmd.Stderr = &stderr
		stdout, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("ttc %s: %w (%s)", argv[0], err, strings.TrimSpace(stderr.String()))
		}
		return string(stdout), nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		_, err := call(cleanup, "rm", "-f", branch)
		if err != nil {
			runErr = errors.Join(runErr, err)
			return
		}
		runErr = errors.Join(runErr, mark("ttc_cleanup", branch))
	}()
	if _, err := call(ctx, "run", "-d", "--pull", "never", "--network", "none", "--memory", "512MiB",
		"--memory-swap", "512MiB", "--cpus", "1", "--name", family, image); err != nil {
		return err
	}
	readToken := func() (string, error) {
		got, err := call(ctx, "exec", branch, "python3", "/pilot/token.py", "read")
		if err != nil {
			return "", err
		}
		wire := strings.TrimSpace(got)
		token, err := hex.DecodeString(wire)
		if err != nil || len(token) != 32 {
			return "", errors.New("TTC returned an invalid calendar session token")
		}
		sum := sha256.Sum256(token)
		return hex.EncodeToString(sum[:]), nil
	}
	before, err := readToken()
	if err != nil {
		return err
	}
	if err := mark("ttc_token_before", before); err != nil {
		return err
	}
	if err := mark("ttc_checkpoint_request", branch); err != nil {
		return err
	}
	if arm == "checkpoint" {
		if _, err := call(ctx, "dehydrate", branch); err != nil {
			return err
		}
	}
	if err := mark("ttc_release", branch); err != nil {
		return err
	}
	hold := time.NewTimer(5 * time.Second)
	defer hold.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-hold.C:
	}
	if err := mark("ttc_hold_end", branch); err != nil {
		return err
	}
	info, err := call(ctx, "inspect", family)
	if err != nil {
		return err
	}
	if !json.Valid([]byte(info)) {
		return errors.New("TTC inspect returned malformed JSON")
	}
	if err := mark("ttc_held", json.RawMessage(info)); err != nil {
		return err
	}
	if err := mark("ttc_restore_request", branch); err != nil {
		return err
	}
	if arm == "checkpoint" {
		if _, err := call(ctx, "rehydrate", branch); err != nil {
			return err
		}
	}
	after, err := readToken()
	if err != nil {
		return err
	}
	if after != before {
		return errors.New("TTC restore changed the calendar session token")
	}
	if err := mark("ttc_token_ready", after); err != nil {
		return err
	}
	if err := harbor.Run(ctx, recording, harbor.Hooks{
		Boundary: func(context.Context, string, int) error { return nil },
		Record:   func(receipt harbor.Receipt) error { return encoder.Encode(receipt) },
		Exec: func(ctx context.Context, c replay.Call) (core.CommandResult, error) {
			return ttcReplayTool(ctx, command, branch, c)
		},
	}); err != nil {
		return err
	}
	if _, err := call(ctx, "cp", tests, branch+":/tests"); err != nil {
		return err
	}
	if _, err := call(ctx, "exec", branch, "bash", "/tests/test.sh"); err != nil {
		return err
	}
	reward, err := call(ctx, "exec", branch, "cat", "/logs/verifier/reward.json")
	if err != nil {
		return err
	}
	var score struct {
		Overall float64 `json:"overall_score"`
	}
	if err := json.Unmarshal([]byte(reward), &score); err != nil {
		return fmt.Errorf("TTC verifier reward: %w", err)
	}
	if err := mark("ttc_verifier", json.RawMessage(reward)); err != nil {
		return err
	}
	if score.Overall != 1 {
		return fmt.Errorf("TTC calendar verifier score %g, want 1", score.Overall)
	}
	hashes, err := call(ctx, "exec", branch, "sha256sum", "results/decision_log.md",
		"results/scheduled.ics", "results/unscheduled.json")
	if err != nil {
		return err
	}
	if err := mark("ttc_hashes", hashes); err != nil {
		return err
	}
	const expected = "4d15946f45c9be84705eaef930d8490ddbae0b1bba2ff2aca6aa0d214debe5a1  results/decision_log.md\n" +
		"d389dc81871c9ebbf2b16ed0e6d7f30b1928e750cd4cd9521c843281d5154d80  results/scheduled.ics\n" +
		"1f14be687f308fdcfcda1534893944dd03f808687f24eaacb941d752525bdb6e  results/unscheduled.json\n"
	if hashes != expected {
		return errors.New("TTC calendar output hashes differ from the reviewed reference")
	}
	return nil
}

func ttcReplayTool(ctx context.Context, command, branch string, c replay.Call) (core.CommandResult, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cwd := c.Workdir
	if cwd == "" {
		cwd = "/"
	}
	args := []string{"exec", "--workdir", cwd, branch}
	switch c.Operation {
	case "ReadFile":
		args = append(args, "cat", c.Path)
	case "Exec":
		args = append(args, "/bin/bash", "-c", c.Script)
	default:
		return core.CommandResult{}, fmt.Errorf("unsupported TTC replay operation %q", c.Operation)
	}
	cmd := exec.CommandContext(ctx, command, args...)
	stdout, err := cmd.CombinedOutput()
	result := core.CommandResult{Stdout: string(stdout)}
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}
