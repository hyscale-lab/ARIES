package vhive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	taskv3 "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/types/known/anypb"
)

// A VM restored by the control service has an adopted Task v3 endpoint but
// no new task in containerd's central task store. The shim owns the framed
// vsock streams and publishes ordinary host-side containerd FIFOs.
func (m *Manager) execModernShim(ctx context.Context, id, execID string, spec *anypb.Any, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
	if !identifier.MatchString(id) {
		return 0, errors.New("invalid shim runtime identity")
	}
	addr, err := os.ReadFile(filepath.Join(m.options.ShimDir, m.options.Namespace+"#"+id, "address"))
	if err != nil {
		return 0, fmt.Errorf("read restored shim address: %w", err)
	}
	socket := strings.TrimPrefix(strings.TrimSpace(string(addr)), "unix://")
	if !filepath.IsAbs(socket) {
		return 0, fmt.Errorf("invalid published shim socket %q", socket)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return 0, err
	}
	client := ttrpc.NewClient(conn)
	defer client.Close()
	task := taskv3.NewTTRPCTaskClient(client)
	var input *execInput
	if stdin != nil {
		input = &execInput{reader: stdin, done: make(chan error, 1)}
		stdin = input
	}
	fs, err := cio.NewCreator(cio.WithStreams(stdin, stdout, stderr), cio.WithFIFODir(m.options.IODir))(execID)
	if err != nil {
		return 0, err
	}
	var release sync.Once
	stop := func() { release.Do(func() { fs.Cancel(); _ = fs.Close() }) }
	cancelled := context.AfterFunc(ctx, stop)
	defer func() { cancelled(); stop(); fs.Wait() }()
	f := fs.Config()
	_, err = task.Exec(ctx, &taskv3.ExecProcessRequest{ID: id, ExecID: execID, Spec: spec, Stdin: f.Stdin, Stdout: f.Stdout, Stderr: f.Stderr})
	if err != nil {
		return 0, fmt.Errorf("shim exec: %w", err)
	}
	_, err = task.Start(ctx, &taskv3.StartRequest{ID: id, ExecID: execID})
	if err != nil {
		return 0, fmt.Errorf("shim start: %w", err)
	}
	if input != nil {
		go func() {
			select {
			case <-ctx.Done():
			case err := <-input.done:
				if err == io.EOF {
					_, _ = task.CloseIO(ctx, &taskv3.CloseIORequest{ID: id, ExecID: execID, Stdin: true})
				}
			}
		}()
	}
	exit, err := task.Wait(ctx, &taskv3.WaitRequest{ID: id, ExecID: execID})
	if err != nil {
		return 0, fmt.Errorf("shim wait: %w", err)
	}
	_, err = task.Delete(ctx, &taskv3.DeleteRequest{ID: id, ExecID: execID})
	if err != nil {
		return 0, fmt.Errorf("shim delete: %w", err)
	}
	fs.Wait()
	return exit.ExitStatus, nil
}
