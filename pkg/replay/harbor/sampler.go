package harbor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"

	"io"
	"os"
	"os/exec"

	"syscall"
	"time"
)

// Start sampling before the session origin and keep the worker process and
// its output owned through teardown. Wait for one real record before admitting tools.
func StartVMMSampler(ctx context.Context, script, socket, output string) (func() error, error) {
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("python3", script, socket)
	cmd.Stderr = os.Stderr
	reader, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err = cmd.Start(); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	ready := make(chan struct{})
	type outcome struct{ stream, process error }
	done := make(chan outcome, 1)
	go func() {
		s := bufio.NewReader(reader)
		line, readErr := s.ReadBytes('\n')
		if readErr == nil {
			var first struct {
				PID  int    `json:"pid"`
				PSS  *int64 `json:"pss_bytes"`
				Time int64  `json:"end_ns"`
			}
			if err := json.Unmarshal(line, &first); err != nil || first.PID <= 0 || first.PSS == nil || first.Time <= 0 {
				readErr = errors.New("invalid first VMM sample")
			}
		}
		if readErr == nil {
			_, readErr = file.Write(line)
		}
		if readErr == nil {
			close(ready)
			_, readErr = io.Copy(file, s)
		}
		if readErr != nil {
			_ = cmd.Process.Kill()
		}
		waitErr := cmd.Wait()
		done <- outcome{errors.Join(readErr, file.Close()), waitErr}
	}()
	stop := func() error {
		select {
		case r := <-done:
			return errors.Join(errors.New("sampler exited before session end"), r.stream, r.process)
		default:
		}
		signalErr := cmd.Process.Signal(syscall.SIGTERM)
		r := <-done
		var exited *exec.ExitError
		if errors.As(r.process, &exited) {
			if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() == syscall.SIGTERM {
				r.process = nil
			}
		}
		return errors.Join(signalErr, r.stream, r.process)
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		return stop, nil
	case r := <-done:
		return nil, errors.Join(errors.New("sampler did not produce a first PSS sample"), r.stream, r.process)
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), stop())
	case <-timer.C:
		return nil, errors.Join(errors.New("sampler timed out before first PSS sample"), stop())
	}
}
