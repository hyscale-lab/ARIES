package vhive

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/protobuf"
	"github.com/containerd/ttrpc"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/hyscale-lab/aries/pkg/core"
)

// A restored guest retains its task manager; a replacement host shim does not.
// Use the guest's existing native task service rather than creating its running
// container again through the host shim.
func (m *Manager) execGuestStream(ctx context.Context, id string, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	started := time.Now()
	failure := func(err error) (core.CommandResult, error) {
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	ctx = m.ctx(ctx)
	if command.Timeout < 0 || command.OutputLimitBytes < 0 || command.OutputLimitBytes > 1<<30 {
		return failure(errors.New("invalid vHive exec timeout or output limit"))
	}
	if command.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, command.Timeout)
		defer cancel()
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	container, err := m.client.LoadContainer(ctx, id)
	if err != nil {
		return failure(err)
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return failure(err)
	}
	if spec.Annotations[vmAnnotation] != id || spec.Process == nil {
		return failure(errors.New("vHive exec target is not a guest container"))
	}
	process, err := execProcess(*spec.Process, command)
	if err != nil {
		return failure(err)
	}
	processAny, err := protobuf.MarshalAnyToProto(&process)
	if err != nil {
		return failure(err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return failure(err)
	}
	// Stay above the host shim's sequential allocation and reserve three ports.
	firstPort := (binary.LittleEndian.Uint32(nonce[:4]) & 0x3fffffff) + 0x40000000
	extra := &proto.ExtraData{RuncOptions: processAny, StdinPort: firstPort, StdoutPort: firstPort + 1, StderrPort: firstPort + 2}
	wrapped, err := protobuf.MarshalAnyToProto(extra)
	if err != nil {
		return failure(err)
	}
	request := &taskAPI.ExecProcessRequest{ID: id, ExecID: "aries-" + hex.EncodeToString(nonce[:]), Spec: wrapped, Stdout: "stdout", Stderr: "stderr"}
	if stdin != nil {
		request.Stdin = "stdin"
	}
	control, err := m.vmControl()
	if err != nil {
		return failure(err)
	}
	info, err := control.GetVMInfo(ctx, &proto.GetVMInfoRequest{VMID: id})
	if err != nil {
		return failure(err)
	}
	socketPath, closeSocketDir, err := shortUnixSocketPath(info.VSockPath)
	if err != nil {
		return failure(err)
	}
	defer closeSocketDir()
	conn, err := vsock.DialContext(ctx, socketPath, 10789)
	if err != nil {
		return failure(fmt.Errorf("connect vHive guest agent: %w", err))
	}
	client := ttrpc.NewClient(conn)
	defer client.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClose()
	limit := command.OutputLimitBytes
	if limit == 0 {
		limit = 16 << 20
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	dial := func(ctx context.Context, port uint32) (net.Conn, error) {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return vsock.DialContext(dialCtx, socketPath, port)
	}
	status, err := runGuestExec(ctx, cancel, taskAPI.NewTaskClient(client), dial, request, extra, stdin,
		&boundedWriter{out: stdout, limit: limit, cancel: cancel}, &boundedWriter{out: stderr, limit: limit, cancel: cancel})
	if err != nil {
		return failure(err)
	}
	return core.CommandResult{ExitCode: int(status), Duration: time.Since(started)}, nil
}

type guestStreamDial func(context.Context, uint32) (net.Conn, error)

func runGuestExec(ctx context.Context, cancel context.CancelCauseFunc, client taskAPI.TaskService, dial guestStreamDial, request *taskAPI.ExecProcessRequest, extra *proto.ExtraData, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
	// Establish streams concurrently with Exec: the guest Exec response waits for
	// its FIFO/vsock pairs to connect. Cancellation closes only local transports.
	started := make(chan struct{})
	streamCtx, stopStreams := context.WithCancel(ctx)
	var streamMu sync.Mutex
	var connections []net.Conn
	closeStreams := func() {
		streamMu.Lock()
		defer streamMu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
	}
	stopClose := context.AfterFunc(streamCtx, closeStreams)
	var pipes sync.WaitGroup
	defer func() {
		stopStreams()
		stopClose()
		closeStreams()
		pipes.Wait()
	}()
	outputs := make(chan struct{})
	stream := func(port uint32, input io.Reader, output io.Writer) {
		conn, err := dial(streamCtx, port)
		if err != nil {
			if streamCtx.Err() == nil {
				cancel(fmt.Errorf("connect vHive exec stream: %w", err))
			}
			return
		}
		defer conn.Close()
		streamMu.Lock()
		if streamCtx.Err() != nil {
			streamMu.Unlock()
			return
		}
		connections = append(connections, conn)
		streamMu.Unlock()
		if input == nil {
			_, err = io.Copy(output, conn)
		} else {
			_, err = io.Copy(conn, input)
			if err == nil {
				select {
				case <-streamCtx.Done():
					return
				case <-started:
				}
				_, err = client.CloseIO(streamCtx, &taskAPI.CloseIORequest{ID: request.ID, ExecID: request.ExecID, Stdin: true})
			}
		}
		if err != nil && streamCtx.Err() == nil {
			cancel(fmt.Errorf("copy vHive exec stream: %w", err))
		}
	}
	pipes.Add(2)
	go func() { defer pipes.Done(); stream(extra.StdoutPort, nil, stdout) }()
	go func() { defer pipes.Done(); stream(extra.StderrPort, nil, stderr) }()
	go func() { pipes.Wait(); close(outputs) }()
	if stdin != nil {
		go stream(extra.StdinPort, stdin, nil)
	}
	if _, err := client.Exec(ctx, request); err != nil {
		return 0, err
	}
	type exitResult struct {
		response *taskAPI.WaitResponse
		err      error
	}
	exited := make(chan exitResult, 1)
	go func() {
		response, err := client.Wait(ctx, &taskAPI.WaitRequest{ID: request.ID, ExecID: request.ExecID})
		exited <- exitResult{response, err}
	}()
	if _, err := client.Start(ctx, &taskAPI.StartRequest{ID: request.ID, ExecID: request.ExecID}); err != nil {
		return 0, err
	}
	close(started)
	var exit exitResult
	select {
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case exit = <-exited:
	}
	if exit.err != nil {
		if ctx.Err() != nil {
			return 0, context.Cause(ctx)
		}
		return 0, exit.err
	}
	select {
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-outputs:
	}
	if ctx.Err() != nil {
		return 0, context.Cause(ctx)
	}
	cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelCleanup()
	if _, err := client.Delete(cleanup, &taskAPI.DeleteRequest{ID: request.ID, ExecID: request.ExecID}); err != nil {
		return 0, fmt.Errorf("release completed vHive guest exec: %w", err)
	}
	return exit.response.ExitStatus, nil
}

// guestTaskRunning checks the retained guest task instead of the replacement
// host shim, whose task table is empty after loading a VM snapshot.
func (m *Manager) guestTaskRunning(ctx context.Context, id string) (bool, error) {
	control, err := m.vmControl()
	if err != nil {
		return false, err
	}
	info, err := control.GetVMInfo(m.ctx(ctx), &proto.GetVMInfoRequest{VMID: id})
	if err != nil {
		return false, err
	}
	socketPath, closeDir, err := shortUnixSocketPath(info.VSockPath)
	if err != nil {
		return false, err
	}
	defer closeDir()
	conn, err := vsock.DialContext(ctx, socketPath, 10789)
	if err != nil {
		return false, err
	}
	client := ttrpc.NewClient(conn)
	defer client.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClose()
	state, err := taskAPI.NewTaskClient(client).State(ctx, &taskAPI.StateRequest{ID: id})
	if err != nil {
		return false, err
	}
	return state.Status == tasktypes.Status_RUNNING, nil
}
