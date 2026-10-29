package vhive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/cio"
	"github.com/hyscale-lab/aries/pkg/core"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func (m *Manager) Exec(ctx context.Context, id string, command core.Command) (core.CommandResult, error) {
	if len(command.Stdin) > 16<<20 {
		return core.CommandResult{ExitCode: -1}, errors.New("vHive exec stdin exceeds 16 MiB")
	}
	var stdout, stderr bytes.Buffer
	var stdin io.Reader
	if len(command.Stdin) != 0 {
		stdin = bytes.NewReader(command.Stdin)
	}
	result, err := m.ExecStream(ctx, id, command, stdin, &stdout, &stderr)
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	return result, err
}

// ExecStream submits argv through containerd into the guest. Cancellation
// releases local IO without killing guest processes needed for evaluation.
func (m *Manager) ExecStream(ctx context.Context, id string, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
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
	c, err := m.client.LoadContainer(ctx, id)
	if err != nil {
		return failure(err)
	}
	spec, err := c.Spec(ctx)
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
	task, err := c.Task(ctx, nil)
	if err != nil {
		return failure(err)
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return failure(err)
	}
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
	bounded := func(w io.Writer) io.Writer { return &boundedWriter{out: w, limit: limit, cancel: cancel} }
	proc, err := task.Exec(ctx, "aries-"+hex.EncodeToString(nonce[:]), &process,
		cio.NewCreator(cio.WithFIFODir(m.options.IODir), cio.WithStreams(stdin, bounded(stdout), bounded(stderr))))
	if err != nil {
		return failure(err)
	}
	var closeOnce sync.Once
	closeIO := func() {
		closeOnce.Do(func() {
			if pipes := proc.IO(); pipes != nil {
				pipes.Cancel()
				_ = pipes.Close()
			}
		})
	}
	stopClose := context.AfterFunc(ctx, closeIO)
	defer func() { stopClose(); closeIO() }()
	exit, err := proc.Wait(ctx)
	if err != nil {
		return failure(err)
	}
	if err = proc.Start(ctx); err != nil {
		return failure(err)
	}
	select {
	case <-ctx.Done():
		return failure(context.Cause(ctx))
	case status := <-exit:
		if status.Error() != nil {
			return failure(status.Error())
		}
		// Drain stdout/stderr before returning or deleting the process's FIFOs.
		if pipes := proc.IO(); pipes != nil {
			pipes.Wait()
		}
		if ctx.Err() != nil {
			return failure(context.Cause(ctx))
		}
		cleanup, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancelCleanup()
		if _, err = proc.Delete(cleanup); err != nil {
			return failure(fmt.Errorf("release completed vHive exec: %w", err))
		}
		return core.CommandResult{ExitCode: int(status.ExitCode()), Duration: time.Since(started)}, nil
	}
}

func execProcess(base specs.Process, c core.Command) (specs.Process, error) {
	if c.Path == "" || strings.ContainsRune(c.Path, 0) {
		return specs.Process{}, errors.New("invalid vHive command path")
	}
	for _, a := range c.Args {
		if strings.ContainsRune(a, 0) {
			return specs.Process{}, errors.New("vHive command argument contains NUL")
		}
	}
	if c.Dir != "" {
		if !path.IsAbs(c.Dir) || path.Clean(c.Dir) != c.Dir || strings.ContainsRune(c.Dir, 0) {
			return specs.Process{}, errors.New("invalid vHive command directory")
		}
		base.Cwd = c.Dir
	}
	base.Args = append([]string{c.Path}, c.Args...)
	base.Terminal = false
	env := map[string]string{}
	for _, v := range base.Env {
		k, v, _ := strings.Cut(v, "=")
		env[k] = v
	}
	if c.User != "" {
		uid, gid, ok := strings.Cut(c.User, ":")
		u, ue := strconv.ParseUint(uid, 10, 32)
		g, ge := strconv.ParseUint(gid, 10, 32)
		if !ok || ue != nil || ge != nil {
			return specs.Process{}, errors.New("vHive command user must be numeric UID:GID")
		}
		if uint32(u) != base.User.UID {
			env["HOME"] = "/"
		}
		base.User = specs.User{UID: uint32(u), GID: uint32(g)}
		if u != 0 {
			base.Capabilities = nil
		}
	}
	for k, v := range c.Env {
		if !validEnv(k) || strings.ContainsRune(v, 0) {
			return specs.Process{}, errors.New("invalid vHive command environment")
		}
		env[k] = v
	}
	base.Env = nil
	for _, k := range slices.Sorted(maps.Keys(env)) {
		base.Env = append(base.Env, k+"="+env[k])
	}
	return base, nil
}

func validEnv(s string) bool {
	for i, r := range s {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return s != ""
}

type boundedWriter struct {
	out            io.Writer
	limit, written int
	cancel         context.CancelCauseFunc
	mu             sync.Mutex
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > w.limit-w.written {
		err := errors.New("vHive exec output limit exceeded")
		w.cancel(err)
		return 0, err
	}
	n, err := w.out.Write(p)
	w.written += n
	if n != len(p) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.cancel(err)
	}
	return n, err
}
