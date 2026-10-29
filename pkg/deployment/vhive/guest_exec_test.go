package vhive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/firecracker-microvm/firecracker-containerd/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

type fakeGuestExec struct {
	taskAPI.TaskService
	exec    func(context.Context, *taskAPI.ExecProcessRequest) (*emptypb.Empty, error)
	start   func(context.Context, *taskAPI.StartRequest) (*taskAPI.StartResponse, error)
	wait    func(context.Context, *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error)
	closeIO func(context.Context, *taskAPI.CloseIORequest) (*emptypb.Empty, error)
	delete  func(context.Context, *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error)
}

func (f *fakeGuestExec) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
	return f.exec(ctx, r)
}
func (f *fakeGuestExec) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	return f.start(ctx, r)
}
func (f *fakeGuestExec) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	return f.wait(ctx, r)
}
func (f *fakeGuestExec) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
	return f.closeIO(ctx, r)
}
func (f *fakeGuestExec) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	return f.delete(ctx, r)
}

func TestGuestExecConnectsBeforeExecReturnsAndClosesStdin(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	extra := &proto.ExtraData{StdinPort: 12000, StdoutPort: 12001, StderrPort: 12002}
	request := &taskAPI.ExecProcessRequest{ID: "sandbox", ExecID: "command", Stdin: "stdin", Stdout: "stdout", Stderr: "stderr"}
	peers := map[uint32]net.Conn{}
	var mu sync.Mutex
	var connected sync.WaitGroup
	connected.Add(3)
	dial := func(_ context.Context, port uint32) (net.Conn, error) {
		host, guest := net.Pipe()
		mu.Lock()
		peers[port] = guest
		mu.Unlock()
		connected.Done()
		return host, nil
	}
	defer func() {
		for _, conn := range peers {
			_ = conn.Close()
		}
	}()
	started, closed, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	deleted := false
	client := &fakeGuestExec{}
	client.exec = func(context.Context, *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
		connected.Wait()
		return &emptypb.Empty{}, nil
	}
	client.start = func(context.Context, *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
		close(started)
		go func() {
			defer close(finished)
			data := make([]byte, 5)
			if _, err := io.ReadFull(peers[extra.StdinPort], data); err != nil || string(data) != "input" {
				cancel(errors.New("stdin bytes lost"))
				return
			}
			select {
			case <-closed:
			case <-ctx.Done():
				return
			}
			_, _ = io.WriteString(peers[extra.StdoutPort], "result")
			_ = peers[extra.StdoutPort].Close()
			_, _ = io.WriteString(peers[extra.StderrPort], "warning")
			_ = peers[extra.StderrPort].Close()
		}()
		return &taskAPI.StartResponse{}, nil
	}
	client.closeIO = func(_ context.Context, r *taskAPI.CloseIORequest) (*emptypb.Empty, error) {
		select {
		case <-started:
		default:
			t.Error("CloseIO preceded Start")
		}
		if !r.Stdin || r.ID != "sandbox" || r.ExecID != "command" {
			t.Error("incorrect stdin close request")
		}
		close(closed)
		return &emptypb.Empty{}, nil
	}
	client.wait = func(ctx context.Context, _ *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
		select {
		case <-finished:
			return &taskAPI.WaitResponse{ExitStatus: 7}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	var stdout, stderr bytes.Buffer
	client.delete = func(context.Context, *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
		if stdout.String() != "result" || stderr.String() != "warning" {
			t.Error("deleted before output drain")
		}
		deleted = true
		return &taskAPI.DeleteResponse{}, nil
	}
	status, err := runGuestExec(ctx, cancel, client, dial, request, extra, bytes.NewBufferString("input"), &stdout, &stderr)
	if err != nil || status != 7 || !deleted {
		t.Fatalf("status=%d deleted=%v err=%v", status, deleted, err)
	}
}

func TestGuestExecCancellationClosesStreamsWithoutDeletingProcess(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var peers [2]net.Conn
	var mu sync.Mutex
	var connected sync.WaitGroup
	connected.Add(2)
	dial := func(_ context.Context, port uint32) (net.Conn, error) {
		host, guest := net.Pipe()
		mu.Lock()
		peers[port-12001] = guest
		mu.Unlock()
		connected.Done()
		return host, nil
	}
	started := make(chan struct{})
	client := &fakeGuestExec{
		exec: func(context.Context, *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
			connected.Wait()
			return &emptypb.Empty{}, nil
		},
		start: func(context.Context, *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
			close(started)
			return &taskAPI.StartResponse{}, nil
		},
		wait: func(ctx context.Context, _ *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		delete: func(context.Context, *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
			t.Error("deleted live guest process on cancellation")
			return nil, errors.New("unexpected delete")
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := runGuestExec(ctx, cancel, client, dial, &taskAPI.ExecProcessRequest{ID: "sandbox", ExecID: "command"}, &proto.ExtraData{StdoutPort: 12001, StderrPort: 12002}, nil, io.Discard, io.Discard)
		done <- err
	}()
	<-started
	cause := errors.New("caller cancelled tool")
	cancel(cause)
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("lost cancellation cause: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("guest exec did not cancel")
	}
	for _, peer := range peers {
		_ = peer.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := peer.Write([]byte("x")); err == nil {
			t.Error("stream remained open after cancellation")
		}
		_ = peer.Close()
	}
}

func TestGuestExecOutputLimitCancelsTransport(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var peers [2]net.Conn
	var connected sync.WaitGroup
	connected.Add(2)
	dial := func(_ context.Context, port uint32) (net.Conn, error) {
		host, guest := net.Pipe()
		peers[port-12001] = guest
		connected.Done()
		return host, nil
	}
	client := &fakeGuestExec{
		exec: func(context.Context, *taskAPI.ExecProcessRequest) (*emptypb.Empty, error) {
			connected.Wait()
			return &emptypb.Empty{}, nil
		},
		start: func(context.Context, *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
			go func() { _, _ = io.WriteString(peers[0], "too much output"); _ = peers[0].Close() }()
			return &taskAPI.StartResponse{}, nil
		},
		wait: func(ctx context.Context, _ *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		delete: func(context.Context, *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
			t.Error("deleted process after output cancellation")
			return nil, errors.New("unexpected delete")
		},
	}
	_, err := runGuestExec(ctx, cancel, client, dial, &taskAPI.ExecProcessRequest{ID: "sandbox", ExecID: "command"}, &proto.ExtraData{StdoutPort: 12001, StderrPort: 12002}, nil, &boundedWriter{out: io.Discard, limit: 4, cancel: cancel}, io.Discard)
	if err == nil || err.Error() != "vHive exec output limit exceeded" {
		t.Fatalf("lost output limit: %v", err)
	}
	for _, peer := range peers {
		_ = peer.Close()
	}
}
