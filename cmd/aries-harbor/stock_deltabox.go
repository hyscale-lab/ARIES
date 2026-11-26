package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

type deltaRPC struct {
	encoder *json.Encoder
	decoder *json.Decoder
}

type deltaResponse struct {
	Error      string          `json:"error"`
	Result     json.RawMessage `json:"result"`
	StartedNS  int64           `json:"started_ns"`
	FinishedNS int64           `json:"finished_ns"`
}

func (rpc deltaRPC) call(op string, fields map[string]any, destination any) (deltaResponse, error) {
	message := map[string]any{"op": op}
	for key, value := range fields {
		message[key] = value
	}
	var response deltaResponse
	if err := rpc.encoder.Encode(message); err != nil {
		return response, fmt.Errorf("DeltaBox %s request: %w", op, err)
	}
	if err := rpc.decoder.Decode(&response); err != nil {
		return response, fmt.Errorf("DeltaBox %s reply: %w", op, err)
	}
	if response.Error != "" {
		return response, fmt.Errorf("DeltaBox %s: %s", op, response.Error)
	}
	if destination != nil {
		if err := json.Unmarshal(response.Result, destination); err != nil {
			return response, fmt.Errorf("DeltaBox %s reply: %w", op, err)
		}
	}
	return response, nil
}

type deltaTool struct {
	Op      string   `json:"op"`
	Argv    []string `json:"argv"`
	CWD     string   `json:"cwd"`
	Timeout float64  `json:"timeout"`
}

type deltaToolResult struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error"`
}

func (rpc deltaRPC) replay(calls []replay.Call) ([]deltaToolResult, error) {
	commands := make([]deltaTool, 0, len(calls))
	for _, call := range calls {
		tool := deltaTool{Op: "exec", CWD: call.Workdir, Timeout: call.Timeout.Seconds()}
		if tool.Timeout == 0 {
			tool.Timeout = 30
		}
		switch call.Operation {
		case "ReadFile":
			tool.Argv, tool.CWD = []string{"/bin/cat", call.Path}, "/root"
		case "Exec":
			tool.Argv = []string{"/bin/bash", "-c", call.Script}
		default:
			return nil, fmt.Errorf("unsupported DeltaBox calendar operation %q", call.Operation)
		}
		commands = append(commands, tool)
	}
	var result struct{ Results []deltaToolResult }
	if _, err := rpc.call("replay", map[string]any{"calls": commands}, &result); err != nil {
		return nil, err
	}
	if len(result.Results) != len(calls) {
		return nil, fmt.Errorf("DeltaBox returned %d replies for %d calls", len(result.Results), len(calls))
	}
	return result.Results, nil
}

func runDeltaBoxCalendar(ctx context.Context, dir, arm string, recording *harbor.Recording) (runErr error) {
	if len(recording.Plan.Calls) != 5 {
		return errors.New("DeltaBox calendar requires the five reviewed calls")
	}
	hashes, err := os.ReadFile(filepath.Join(dir, "result-hashes.txt"))
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "python3", "-u", filepath.Join(dir, "serve.py"))
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = stdin.Close()
		runErr = errors.Join(runErr, cmd.Wait())
	}()
	rpc := deltaRPC{json.NewEncoder(stdin), json.NewDecoder(stdout)}
	var startup struct {
		Event   string
		Profile json.RawMessage
		CPUs    int `json:"guest_cpu_count"`
	}
	if _, err := rpc.call("ready", nil, &startup); err != nil {
		return err
	}
	if startup.Event != "ready" || startup.CPUs != 1 {
		return errors.New("DeltaBox appliance failed readiness or one-vCPU validation")
	}
	type probeState struct {
		Token       string
		PID         int
		InnerPID    int `json:"inner_pid"`
		Profile     json.RawMessage
		Workspace   bool   `json:"workspace_rollback"`
		RestorePath string `json:"restore_path"`
	}
	var initial, restored probeState
	if _, err := rpc.call("token", nil, &initial); err != nil {
		return err
	}
	if initial.PID <= 0 || len(initial.Token) != 32 {
		return errors.New("invalid DeltaBox in-memory probe")
	}
	sample := func(phase string) (deltaResponse, error) {
		return rpc.call("memory", map[string]any{"phase": phase}, nil)
	}
	event := func(name string, fields map[string]any) (deltaResponse, error) {
		return rpc.call("event", map[string]any{"event": name, "fields": fields}, nil)
	}
	if _, err := sample("pre_capture"); err != nil {
		return err
	}
	start, err := event("arm_start", map[string]any{"arm": arm})
	if err != nil {
		return err
	}
	var checkpoint deltaResponse
	var released int64 = start.FinishedNS
	restoreOp := "token"
	if arm == "checkpoint" {
		var receipt struct {
			StoppedPID int `json:"worker_stopped_pid"`
		}
		checkpoint, err = rpc.call("checkpoint", nil, &receipt)
		if err != nil {
			return err
		}
		if receipt.StoppedPID != initial.PID {
			return errors.New("DeltaBox checkpoint did not stop the probed worker")
		}
		released, restoreOp = checkpoint.FinishedNS, "restore"
	}
	holdStart, err := sample("hold_start")
	if err != nil {
		return err
	}
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	holdEnd, err := sample("hold_end")
	if err != nil {
		return err
	}
	restore, err := rpc.call(restoreOp, nil, &restored)
	if err != nil {
		return err
	}
	if initial.Token != restored.Token || initial.InnerPID != 100 || initial.InnerPID != restored.InnerPID ||
		(initial.PID == restored.PID) != (arm == "control") {
		return errors.New("DeltaBox changed the probed in-memory state")
	}
	if arm == "checkpoint" && (restored.RestorePath != "warm-template" || !restored.Workspace || !bytes.Equal(startup.Profile, restored.Profile)) {
		return errors.New("DeltaBox rollback or guest profile verification failed")
	}
	if _, err := sample("post_restore"); err != nil {
		return err
	}
	var batch []deltaToolResult
	var batchErr error
	var arrival sync.Mutex
	var arrived int
	batchDone := make(chan struct{})
	var first, last int64
	trace := json.NewEncoder(os.Stdout)
	if err := harbor.Run(ctx, recording, harbor.Hooks{
		Boundary: func(context.Context, string, int) error { return nil },
		Record: func(receipt harbor.Receipt) error {
			if receipt.Event == "tool_dispatch" && receipt.Sequence == 1 {
				first = receipt.Time
			}
			if receipt.Event == "tool_reply" && receipt.Sequence <= 3 {
				last = max(last, receipt.Time)
			}
			return trace.Encode(receipt)
		},
		Exec: func(callCtx context.Context, call replay.Call) (core.CommandResult, error) {
			var result deltaToolResult
			if call.Sequence <= 3 {
				arrival.Lock()
				arrived++
				last := arrived == 3
				arrival.Unlock()
				if last {
					batch, batchErr = rpc.replay(recording.Plan.Calls[:3])
					close(batchDone)
				}
				select {
				case <-callCtx.Done():
					return core.CommandResult{}, callCtx.Err()
				case <-batchDone:
				}
				if batchErr != nil {
					return core.CommandResult{}, batchErr
				}
				result = batch[call.Sequence-1]
			} else {
				results, err := rpc.replay([]replay.Call{call})
				if err != nil {
					return core.CommandResult{}, err
				}
				result = results[0]
			}
			if result.Error != "" {
				return core.CommandResult{}, errors.New(result.Error)
			}
			return core.CommandResult{ExitCode: result.Exit, Stdout: result.Stdout, Stderr: result.Stderr}, nil
		},
	}); err != nil {
		return err
	}
	end, err := event("replay_end", nil)
	if err != nil {
		return err
	}
	var verified struct {
		Hashes string
		Reward map[string]any
	}
	if _, err := rpc.call("verify", nil, &verified); err != nil {
		return err
	}
	if verified.Reward["overall_score"] != 1.0 || strings.TrimSpace(verified.Hashes) != strings.TrimSpace(string(hashes)) {
		return errors.New("DeltaBox calendar score or output hash mismatch")
	}
	_, err = event("verified", map[string]any{
		"arm": arm, "arm_start_ns": start.FinishedNS, "checkpoint_start_ns": checkpoint.StartedNS,
		"release_ns": released, "hold_start_ns": holdStart.FinishedNS, "hold_end_ns": holdEnd.FinishedNS,
		"restore_ns": restore.StartedNS, "token_read_ns": restore.FinishedNS, "first_batch_start_ns": first,
		"first_batch_end_ns": last, "replay_end_ns": end.FinishedNS, "reward": verified.Reward, "hashes": verified.Hashes,
	})
	if err != nil {
		return err
	}
	_, err = rpc.call("shutdown", nil, nil)
	return err
}
