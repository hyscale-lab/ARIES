package apptainer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	maxExecInput        = 16 << 20
	maxExecOutput       = 16 << 20
	maxConfiguredOutput = 1 << 30
	execTrailerKeep     = 512
	// execShell reports the command's exit status in a trailer, because
	// apptainer's own failures also exit nonzero; a working directory that does
	// not exist is exit 126, as on Kubernetes. The command's shell, not
	// this one, reads the environment file, so a variable cannot overwrite the
	// wrapper's own.
	execShell    = `token=$1; dir=$2; envfile=$3; shift 3; if [ -n "$dir" ] && ! cd "$dir"; then printf '\036ARIES_EXEC_EXIT_%s=126\037' "$token" >&2; exit 126; fi; /bin/sh -c 'set -a; . "$1"; set +a; shift; exec "$@"' aries-env "$envfile" "$@"; status=$?; printf '\036ARIES_EXEC_EXIT_%s=%d\037' "$token" "$status" >&2; exit "$status"`
	rootExecUser = "0:0"
)

// Exec runs one argv to completion with buffered output. Nonzero exits are
// returned as results, not transport errors.
func (manager *Manager) Exec(ctx context.Context, id string, command core.Command) (core.CommandResult, error) {
	started := time.Now()
	if len(command.Stdin) > maxExecInput {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("Apptainer exec stdin exceeds %d bytes", maxExecInput)
	}
	var stdout, stderr bytes.Buffer
	var stdin io.Reader
	if len(command.Stdin) > 0 {
		stdin = bytes.NewReader(command.Stdin)
	}
	result, err := manager.ExecStream(ctx, id, command, stdin, &stdout, &stderr)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	return result, err
}

// ExecStream preserves the exact argv and streams I/O. Cancellation and
// timeouts stop the local apptainer process; as with Docker and Kubernetes,
// the command may continue until it exits or Stop removes the instance.
//
// Every command runs as the instance's root, which is the invoking user with
// the appearance of root: without subordinate IDs an unprivileged user cannot
// switch to another UID, so a command for any other user is refused.
func (manager *Manager) ExecStream(ctx context.Context, id string, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	started := time.Now()
	failure := func() core.CommandResult { return core.CommandResult{ExitCode: -1, Duration: time.Since(started)} }
	if err := validateCommand(command); err != nil {
		return failure(), err
	}
	current, _, err := manager.loadRecord(id)
	if err != nil {
		return failure(), err
	}
	outputLimit := command.OutputLimitBytes
	if outputLimit == 0 {
		outputLimit = maxExecOutput
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	execCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if command.Timeout > 0 {
		var cancelTimeout context.CancelFunc
		execCtx, cancelTimeout = context.WithTimeout(execCtx, command.Timeout)
		defer cancelTimeout()
	}
	token, err := randomID()
	if err != nil {
		return failure(), fmt.Errorf("generate Apptainer exec token: %w", err)
	}
	// The runtime's environment and the command's own travel in a private file:
	// on apptainer's command line, values would be visible to every user of the
	// host.
	environment, err := manager.commandEnvironment(id, token, command.Env)
	if err != nil {
		return failure(), err
	}
	defer func() { _ = os.Remove(environment) }()

	args := append(execPrefix(id, current.Workdir), "/bin/sh", "-c", execShell, "aries-exec", token, command.Dir, shareTarget+"/"+filepath.Base(environment), command.Path)
	args = append(args, command.Args...)

	// An exceeded limit cancels apptainer; otherwise it would block on a pipe
	// that nobody reads.
	exceeded := func() { cancel(fmt.Errorf("Apptainer exec output exceeds %d bytes", outputLimit)) }
	stdoutWriter := &limitedWriter{writer: stdout, limit: outputLimit, onExceed: exceeded}
	stderrWriter := &limitedWriter{writer: stderr, limit: outputLimit, onExceed: exceeded}
	trailer := newExitTrailer(stderrWriter, token)
	runErr := manager.apptainer.Run(execCtx, stdin, stdoutWriter, trailer, args...)

	if execCtx.Err() != nil {
		return failure(), context.Cause(execCtx)
	}
	exitCode, err := trailer.Finish()
	if err != nil {
		return failure(), errors.Join(err, runErr)
	}
	// The trailer reports how the command ended; its output may still have failed
	// to reach the caller.
	if err := errors.Join(stdoutWriter.err(), stderrWriter.err()); err != nil {
		return failure(), fmt.Errorf("deliver Apptainer exec output: %w", err)
	}
	return core.CommandResult{ExitCode: exitCode, Duration: time.Since(started)}, nil
}

// commandEnvironment writes the runtime's environment followed by the
// command's, as Docker gives an exec the container's environment plus its own,
// and returns the file's host path.
func (manager *Manager) commandEnvironment(id, token string, overrides map[string]string) (string, error) {
	base, err := os.ReadFile(filepath.Join(manager.sharePath(id), environmentFile))
	if err != nil {
		return "", fmt.Errorf("read deployment environment: %w", err)
	}
	path := filepath.Join(manager.sharePath(id), "exec-"+token+".env")
	if err := writePrivate(path, append(base, environmentScript(nil, overrides)...)); err != nil {
		return "", fmt.Errorf("write exec environment: %w", err)
	}
	return path, nil
}

// UploadArchive extracts a tar stream beneath destination. Without subordinate
// IDs the instance's root cannot give files to other users, so extracted files
// belong to root; modes are kept. Harnesses verify effective permissions.
func (manager *Manager) UploadArchive(ctx context.Context, id, destination string, archive io.Reader) error {
	if destination == "" || !strings.HasPrefix(destination, "/") || strings.ContainsRune(destination, 0) {
		return errors.New("deployment upload destination must be absolute")
	}
	if _, _, err := manager.loadRecord(id); err != nil {
		return err
	}
	if _, err := manager.run(ctx, archive, append(execPrefix(id, "/"), "tar", "-x", "-o", "-f", "-", "-C", destination)...); err != nil {
		return fmt.Errorf("extract archive in Apptainer instance: %w", err)
	}
	return nil
}

// DownloadArchive returns a tar stream of source, named by its base, with the
// source's own metadata.
func (manager *Manager) DownloadArchive(ctx context.Context, id, source string) (io.ReadCloser, deployment.FileInfo, error) {
	if source == "" || !strings.HasPrefix(source, "/") || strings.ContainsRune(source, 0) || path.Clean(source) != source || source == "/" {
		return nil, deployment.FileInfo{}, errors.New("invalid deployment download source: path must be absolute, clean and not the root")
	}
	stat, err := manager.Exec(ctx, id, core.Command{Path: "stat", Args: []string{"-c", "%s %f", source}})
	if err != nil {
		return nil, deployment.FileInfo{}, fmt.Errorf("inspect deployment archive source: %w", err)
	}
	if stat.ExitCode != 0 {
		// Exec succeeded, so the instance exists and only the source is missing.
		if strings.Contains(stat.Stderr, "No such file") {
			return nil, deployment.FileInfo{}, fmt.Errorf("deployment archive: %w: %s", runner.ErrNotFound, strings.TrimSpace(stat.Stderr))
		}
		return nil, deployment.FileInfo{}, fmt.Errorf("inspect deployment archive source: %s", strings.TrimSpace(stat.Stderr))
	}
	info, err := parseStat(stat.Stdout)
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	go func() {
		var stderr bytes.Buffer
		err := manager.apptainer.Run(streamCtx, nil, writer, &stderr, append(execPrefix(id, "/"), "tar", "-c", "-f", "-", "-C", path.Dir(source), path.Base(source))...)
		if err != nil {
			err = fmt.Errorf("archive Apptainer instance path: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		_ = writer.CloseWithError(err)
	}()
	return &archiveStream{PipeReader: reader, cancel: cancel}, info, nil
}

// execPrefix runs a command in the instance with a clean environment and an
// explicit working directory: without one, apptainer tries the host's current
// directory and prints a warning into the command's stderr.
func execPrefix(id, workdir string) []string {
	if workdir == "" {
		workdir = "/"
	}
	return []string{"exec", "--cleanenv", "--pwd", workdir, "instance://" + id}
}

type archiveStream struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (stream *archiveStream) Close() error {
	stream.cancel()
	return stream.PipeReader.Close()
}

// parseStat reads `stat -c '%s %f'`: a decimal size and the raw mode in hex.
func parseStat(output string) (deployment.FileInfo, error) {
	fields := strings.Fields(output)
	if len(fields) != 2 {
		return deployment.FileInfo{}, fmt.Errorf("unexpected stat output %q", output)
	}
	size, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return deployment.FileInfo{}, fmt.Errorf("parse stat size: %w", err)
	}
	raw, err := strconv.ParseUint(fields[1], 16, 32)
	if err != nil {
		return deployment.FileInfo{}, fmt.Errorf("parse stat mode: %w", err)
	}
	mode := fs.FileMode(raw & 0o777)
	switch raw & 0o170000 {
	case 0o100000:
	case 0o040000:
		mode |= fs.ModeDir
	case 0o120000:
		mode |= fs.ModeSymlink
	default:
		mode |= fs.ModeIrregular
	}
	return deployment.FileInfo{Size: size, Mode: mode}, nil
}

// exitTrailerWriter forwards stderr while withholding the wrapper's trailer.
// apptainer may append its own message after the trailer; that text is dropped.
type exitTrailerWriter struct {
	destination io.Writer
	prefix      []byte
	buffer      bytes.Buffer
}

func newExitTrailer(destination io.Writer, token string) *exitTrailerWriter {
	return &exitTrailerWriter{destination: destination, prefix: []byte("\x1eARIES_EXEC_EXIT_" + token + "=")}
}

func (w *exitTrailerWriter) Write(content []byte) (int, error) {
	written, _ := w.buffer.Write(content)
	if excess := w.buffer.Len() - execTrailerKeep; excess > 0 {
		chunk := w.buffer.Next(excess)
		if n, err := w.destination.Write(chunk); err != nil {
			return 0, err
		} else if n != len(chunk) {
			return 0, io.ErrShortWrite
		}
	}
	return written, nil
}

func (w *exitTrailerWriter) Finish() (int, error) {
	content := w.buffer.Bytes()
	start := bytes.LastIndex(content, w.prefix)
	if start < 0 {
		return -1, fmt.Errorf("Apptainer exec output is missing its exit trailer: %s", strings.TrimSpace(string(content)))
	}
	rest := content[start+len(w.prefix):]
	end := bytes.IndexByte(rest, '\x1f')
	if end < 0 {
		return -1, errors.New("Apptainer exec output has an invalid exit trailer")
	}
	exitCode, err := strconv.Atoi(string(rest[:end]))
	if err != nil || exitCode < 0 || exitCode > 255 {
		return -1, errors.New("Apptainer exec output has an invalid exit code")
	}
	if _, err := w.destination.Write(content[:start]); err != nil {
		return -1, fmt.Errorf("write Apptainer exec stderr: %w", err)
	}
	return exitCode, nil
}

type limitedWriter struct {
	writer   io.Writer
	wrote    int
	limit    int
	once     sync.Once
	onExceed func()
	// failed is the destination's first error, read after the command.
	mu     sync.Mutex
	failed error
}

func (w *limitedWriter) Write(content []byte) (int, error) {
	if len(content) > w.limit-w.wrote {
		w.once.Do(w.onExceed)
		return 0, fmt.Errorf("Apptainer exec output exceeds %d bytes", w.limit)
	}
	written, err := w.writer.Write(content)
	w.wrote += written
	if err != nil {
		w.mu.Lock()
		if w.failed == nil {
			w.failed = err
		}
		w.mu.Unlock()
	}
	return written, err
}

func (w *limitedWriter) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

func validateCommand(command core.Command) error {
	if command.Path == "" || strings.ContainsRune(command.Path, 0) {
		return errors.New("command path must be nonempty and NUL-free")
	}
	if command.Dir != "" {
		if !strings.HasPrefix(command.Dir, "/") || strings.ContainsRune(command.Dir, 0) || path.Clean(command.Dir) != command.Dir {
			return errors.New("invalid command workdir: path must be absolute, clean, and NUL-free")
		}
	}
	if command.User != "" && command.User != rootExecUser {
		return fmt.Errorf("Apptainer runs every command as the instance's root and cannot run one as %q", command.User)
	}
	if command.Timeout < 0 {
		return errors.New("command timeout must be nonnegative")
	}
	if command.OutputLimitBytes < 0 || command.OutputLimitBytes > maxConfiguredOutput {
		return fmt.Errorf("command output limit must be between 0 and %d bytes", maxConfiguredOutput)
	}
	for _, argument := range command.Args {
		if strings.ContainsRune(argument, 0) {
			return errors.New("command argument contains NUL")
		}
	}
	for key, value := range command.Env {
		if !validEnvName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid command environment %q", key)
		}
	}
	return nil
}

func validEnvName(value string) bool {
	for index, r := range value {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}

func randomID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
