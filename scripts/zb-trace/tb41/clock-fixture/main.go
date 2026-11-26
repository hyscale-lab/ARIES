// clock-fixture exercises the Harbor scheduler with two safe commands in a restored VM.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func main() {
	var options vhive.Options
	flag.StringVar(&options.Socket, "socket", "", "isolated AgentENV containerd socket")
	flag.StringVar(&options.CheckpointDir, "checkpoint-dir", "", "runtime checkpoint directory")
	flag.StringVar(&options.ShimDir, "shim-dir", "", "runtime shim directory")
	flag.StringVar(&options.IODir, "io-dir", "", "runtime I/O directory")
	image := flag.String("image", "", "available pinned task image")
	traced := flag.Bool("traced", false, "exercise real Firecracker pauses")
	flag.Parse()
	if options.Socket == "" || options.CheckpointDir == "" || options.ShimDir == "" || options.IODir == "" || *image == "" {
		flag.Usage()
		os.Exit(2)
	}
	options.Namespace = "aries"
	options.IdleMode = "checkpoint"
	options.GuestTaskV3 = true
	options.NativeCheckpoint = true
	options.VCPUs, options.MemoryMB = 2, 4096
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, 3*time.Minute)
	defer cancel()
	if err := run(ctx, options, *image, *traced, json.NewEncoder(os.Stdout)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, options vhive.Options, image string, traced bool, log *json.Encoder) (retErr error) {
	const modeledWait = 12 * time.Second
	m, err := vhive.New(options)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, m.Close()) }()
	name := fmt.Sprintf("zb-clock-%d", time.Now().UnixNano())
	report := func(event string) error {
		at, err := harbor.Clock()
		if err != nil {
			return err
		}
		return log.Encode(map[string]any{"event": event, "monotonic_ns": at, "runtime_id": name})
	}
	req := deployment.Request{
		Name: name, Image: image, Workdir: "/tmp", StorageMB: 10240,
		Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"},
		Labels: map[string]string{"aries.managed": "true", "aries.component": "sandbox", "aries.kind": "task-container", "aries.run": name, "aries.task": "clock-fixture"},
	}
	if err := log.Encode(map[string]any{
		"event": "fixture", "image": image, "vcpus": 2, "memory_mib": 4096, "storage_mib": 10240,
		"source_tool_ns": int64(20 * time.Second), "modeled_wait_ns": int64(modeledWait),
		"has_evaluator": false, "traced": traced,
	}); err != nil {
		return err
	}
	if err := report("create_start"); err != nil {
		return err
	}
	id, err := m.Create(ctx, req)
	if id != "" {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := m.Stop(cleanup, id); err != nil {
				retErr = errors.Join(retErr, err)
			} else {
				retErr = errors.Join(retErr, report("removed"))
			}
		}()
	}
	if err != nil {
		return err
	}
	if err := m.Validate(ctx, id, req, nil); err != nil {
		return err
	}
	guest := func(ctx context.Context, script string) (core.CommandResult, error) {
		return m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", script}, Dir: "/tmp"})
	}
	ready, err := guest(ctx, "printf preserved > /tmp/clock-fixture-ready && test -s /tmp/clock-fixture-ready")
	if err != nil || ready.ExitCode != 0 {
		return errors.Join(err, fmt.Errorf("guest readiness exited %d", ready.ExitCode))
	}
	if err := report("readiness_passed"); err != nil {
		return err
	}
	if err := m.Start(ctx, id); err != nil {
		return err
	}
	for _, stage := range []struct {
		name string
		run  func(context.Context, string) error
	}{{"snapshot", m.Suspend}, {"restore", m.Resume}} {
		if err := report(stage.name + "_start"); err != nil {
			return err
		}
		if err := stage.run(ctx, id); err != nil {
			return err
		}
		if err := report(stage.name + "_complete"); err != nil {
			return err
		}
	}
	socket, err := m.VMMSocket(ctx, id)
	if err != nil {
		return err
	}
	calls := []replay.Call{
		{Sequence: 1, Operation: "Exec", Script: "sleep 13; cat /tmp/clock-fixture-ready", ResponseStdout: []byte("preserved")},
		{Sequence: 2, Operation: "Exec", Script: "grep -qx preserved /tmp/clock-fixture-ready && printf complete", ResponseStdout: []byte("complete")},
	}
	recording := &harbor.Recording{Plan: replay.Plan{Calls: calls}, Events: []harbor.Event{
		{Kind: "exec_command", Step: 1, CallID: "long", Sequence: 1},
		{Kind: "function_call_output", Step: 1, CallID: "long", Sequence: 1},
		{Kind: "exec_command", Step: 2, CallID: "readback", Sequence: 2, Wait: modeledWait},
		{Kind: "function_call_output", Step: 2, CallID: "readback", Sequence: 2},
	}}
	hooks := harbor.Hooks{
		Exec: func(ctx context.Context, call replay.Call) (core.CommandResult, error) {
			res, err := guest(ctx, call.Script)
			if err == nil && (res.ExitCode != 0 || string(call.ResponseStdout) != res.Stdout) {
				err = fmt.Errorf("fixture reply mismatch, call %d, exit %d, stdout %q", call.Sequence, res.ExitCode, res.Stdout)
			}
			return res, err
		},
		Record: func(event harbor.Receipt) error { return log.Encode(event) },
		Periodic: func(context.Context, string, int, []string) (harbor.Freeze, error) {
			return harbor.Freeze{}, nil
		},
	}
	if traced {
		dir, err := os.Open(filepath.Dir(socket))
		if err != nil {
			return err
		}
		defer dir.Close()
		short := fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), filepath.Base(socket))
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", short)
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		hooks.Periodic = func(ctx context.Context, phase string, step int, triggers []string) (harbor.Freeze, error) {
			return capture(ctx, client, phase, step, triggers, log)
		}
	}
	return harbor.Run(ctx, recording, hooks)
}

func patch(ctx context.Context, client *http.Client, state string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "http://localhost/vm", strings.NewReader(`{"state":"`+state+`"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.Join(fmt.Errorf("Firecracker %s: %s %s", state, resp.Status, body), err)
	}
	return nil
}

func capture(ctx context.Context, client *http.Client, phase string, step int, triggers []string, log *json.Encoder) (harbor.Freeze, error) {
	var span harbor.Freeze
	pauseRequest, err := harbor.Clock()
	if err != nil {
		return span, err
	}
	pauseErr := patch(ctx, client, "Paused")
	span.PauseAckNS, err = harbor.Clock()
	freezeErr := err
	if pauseErr == nil && freezeErr == nil {
		time.Sleep(180 * time.Millisecond)
	}
	resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	span.ResumeRequestNS, err = harbor.Clock()
	resumeErr := patch(resumeCtx, client, "Resumed")
	resumeAck, ackErr := harbor.Clock()
	if err := errors.Join(pauseErr, freezeErr, err, resumeErr, ackErr); err != nil {
		return harbor.Freeze{}, err
	}
	if phase == "post-tool" && step == 1 {
		time.Sleep(300 * time.Millisecond)
	}
	exportEnd, err := harbor.Clock()
	return span, errors.Join(err, log.Encode(map[string]any{
		"event": "fixture_capture", "phase": phase, "step_id": step, "triggers": triggers,
		"pause_request_monotonic_ns": pauseRequest, "pause_ack_monotonic_ns": span.PauseAckNS,
		"resume_request_monotonic_ns": span.ResumeRequestNS, "resume_ack_monotonic_ns": resumeAck,
		"export_start_monotonic_ns": resumeAck, "export_end_monotonic_ns": exportEnd,
		"pause_http_status": 204, "resume_http_status": 204,
	}))
}
