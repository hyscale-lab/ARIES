// Command tsb-pages-vm exposes native sandbox operations as JSON lines for a
// host-side page observer. No model or observer code runs inside the driver.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/namespaces"
	fcclient "github.com/firecracker-microvm/firecracker-containerd/firecracker-control/client"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"golang.org/x/sys/unix"
)

type request struct {
	ID             string            `json:"id"`
	Op             string            `json:"op,omitempty"`
	Argv           []string          `json:"argv,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds float64           `json:"timeout_seconds,omitempty"`
}

func mono() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic(err)
	}
	return ts.Nano()
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() (retErr error) {
	output := flag.String("output", "", "new private artifact directory")
	image := flag.String("image", "docker.io/library/alpine:3.21.3", "preimported image")
	idle := flag.String("idle-mode", "persistent", "persistent or checkpoint; explicit suspend/resume operations")
	timeout := flag.Duration("timeout", 30*time.Minute, "whole driver deadline")
	cni := flag.String("cni-network", "", "optional configured CNI network")
	uffd := flag.String("uffd-socket", "", "native UFFD memory-backend socket for checkpoint restore")
	checkpointDir := flag.String("checkpoint-dir", "/var/lib/firecracker-containerd/aries-checkpoints", "native checkpoint directory")
	flag.Parse()
	if *output == "" {
		return errors.New("-output is required")
	}
	if *idle != "persistent" && *idle != "checkpoint" {
		return errors.New("invalid -idle-mode")
	}
	if err := os.Mkdir(*output, 0700); err != nil {
		return err
	}
	write := func(name string, v any) error {
		b, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(*output, name), append(b, '\n'), 0600)
	}
	evidence, err := os.OpenFile(filepath.Join(*output, "driver.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer evidence.Close()
	emit := func(v any) error {
		b, e := json.Marshal(v)
		if e != nil {
			return e
		}
		b = append(b, '\n')
		if _, e = evidence.Write(b); e != nil {
			return e
		}
		_, e = os.Stdout.Write(b)
		return e
	}
	base, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(base, *timeout)
	defer cancel()
	m, err := vhive.New(vhive.Options{VCPUs: 2, MemoryMB: 4096, IdleMode: *idle, CNINetwork: *cni, RestoreMemorySocket: *uffd, CheckpointDir: *checkpointDir})
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, m.Close()) }()
	cpu, mem := 2.0, 4096
	name := fmt.Sprintf("aries-tsb-pages-%d", time.Now().UnixNano())
	req := deployment.Request{Name: name, Image: *image, Workdir: "/", CPU: &cpu, MemoryMB: &mem, StorageMB: 10240, Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "tsb-tool-pages", "aries.task": "page-comparison"}}
	if err = write("request.json", req); err != nil {
		return err
	}
	id, createErr := m.Create(ctx, req)
	checkpointState := filepath.Join(*checkpointDir, "aries", id, "state.json")
	if id != "" {
		defer func() {
			cctx, c := context.WithTimeout(context.Background(), 2*time.Minute)
			defer c()
			e := m.Stop(cctx, id)
			status, msg := "confirmed", ""
			if e != nil {
				status, msg = "failed", e.Error()
			}
			retErr = errors.Join(retErr, e, write("cleanup.json", map[string]any{"runtime_id": id, "status": status, "error": msg, "time": time.Now().UTC()}))
		}()
	}
	if createErr != nil {
		return createErr
	}
	if err = m.Validate(ctx, id, req, nil); err != nil {
		return err
	}
	if err = m.Start(ctx, id); err != nil {
		return err
	}
	control, err := fcclient.New("/run/firecracker-containerd/containerd.sock.ttrpc")
	if err != nil {
		return err
	}
	defer control.Close()
	vmInfo := func() (*proto.GetVMInfoResponse, error) {
		return control.GetVMInfo(namespaces.WithNamespace(ctx, "aries"), &proto.GetVMInfoRequest{VMID: id})
	}
	info, err := vmInfo()
	if err != nil {
		return err
	}
	if err = write("vm-info.json", info); err != nil {
		return err
	}
	guest, err := m.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", "uname -r; cat /proc/sys/kernel/random/boot_id; getconf _NPROCESSORS_ONLN; cat /proc/meminfo"}})
	if err != nil {
		return err
	}
	if err = write("guest.json", guest); err != nil {
		return err
	}
	obs, obsErr := m.ObserveMemory(ctx, id)
	if obsErr != nil {
		return obsErr
	}
	if err = write("initial-memory.json", obs); err != nil {
		return err
	}
	if obs.MachineConfiguration.VCPUCount != 2 || obs.MachineConfiguration.MemoryMiB != 4096 {
		return errors.New("actual VM configuration differs from 2 vCPUs/4096 MiB")
	}
	if err = emit(map[string]any{"event": "ready", "vm_id": id, "vm_info": info, "machine_configuration": obs.MachineConfiguration, "processes": obs.Processes, "host_monotonic_ns": mono(), "idle_mode": *idle, "checkpoint_state": checkpointState}); err != nil {
		return err
	}
	// A reader goroutine lets cancellation clean up even while stdin is idle.
	type input struct {
		line []byte
		err  error
	}
	lines := make(chan input)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 65536), 8<<20)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- input{line: line}:
			case <-ctx.Done():
				return
			}
		}
		e := scanner.Err()
		if e == nil {
			e = io.EOF
		}
		select {
		case lines <- input{err: e}:
		case <-ctx.Done():
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case in := <-lines:
			if in.err == io.EOF {
				return nil
			}
			if in.err != nil {
				return in.err
			}
			var call request
			if err = json.Unmarshal(in.line, &call); err != nil {
				return fmt.Errorf("decode request: %w", err)
			}
			if call.ID == "" {
				return errors.New("request id required")
			}
			if call.Op == "" {
				call.Op = "exec"
			}
			begin := mono()
			started := time.Now().UTC()
			response := map[string]any{"event": "result", "id": call.ID, "op": call.Op, "vm_id": id, "begin_monotonic_ns": begin, "started_at": started}
			var opErr error
			switch call.Op {
			case "exec":
				if len(call.Argv) == 0 || call.Argv[0] == "" {
					opErr = errors.New("nonempty argv required")
					break
				}
				seconds := call.TimeoutSeconds
				if seconds == 0 {
					seconds = 600
				}
				if seconds <= 0 || seconds > 1800 {
					opErr = errors.New("timeout_seconds must be in (0,1800]")
					break
				}
				command := core.Command{Path: call.Argv[0], Args: call.Argv[1:], Dir: call.Cwd, Env: call.Env, Timeout: time.Duration(seconds * float64(time.Second)), OutputLimitBytes: 8 << 20}
				tctx, timing := core.WithCommandTimings(ctx)
				var result core.CommandResult
				result, opErr = m.Exec(tctx, id, command)
				response["exit_code"], response["stdout"], response["stderr"] = result.ExitCode, result.Stdout, result.Stderr
				response["command_duration_ns"], response["command_timing"] = result.Duration.Nanoseconds(), timing.Snapshot()
			case "suspend":
				opErr = m.Suspend(ctx, id)
				response["checkpoint_state"] = checkpointState
			case "resume":
				opErr = m.Resume(ctx, id)
				if opErr == nil {
					var v *proto.GetVMInfoResponse
					v, opErr = vmInfo()
					response["vm_info"] = v
				}
			default:
				opErr = fmt.Errorf("unsupported operation %q", call.Op)
			}
			response["end_monotonic_ns"], response["finished_at"] = mono(), time.Now().UTC()
			response["error"] = ""
			if opErr != nil {
				response["error"] = opErr.Error()
			}
			if err = emit(response); err != nil {
				return err
			}
		}
	}
}
