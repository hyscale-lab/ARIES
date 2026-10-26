package kubernetes

// The file capability a borrowed bridge target asserts on a pod: stat, read a
// file as a stream, and write one atomically. kubectl exec is the bridge's only
// data path, so each operation is a short POSIX script. The scripts test their
// conditions and choose their own exit codes; stderr text is only a fallback.
//
// Reads act as the container user, root on every current benchmark image, as
// Docker's archive API does. A write acts as the user of the agent's shell:
// the exec user through setpriv, or the container user when none is set.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// Exit codes the scripts choose. kubectl itself exits 1 on its own failures.
const (
	notDirectoryExit = 20 // ENOTDIR
	isDirectoryExit  = 21 // EISDIR
	missingExit      = 66 // sysexits' EX_NOINPUT
	refusedExit      = 77 // sysexits' EX_NOPERM
)

// statScript prints a path's size and raw mode, following a symlink. A path
// under a file or a dangling symlink does not exist, as to `[ -e ]`.
const statScript = `export LC_ALL=C
stat -L -c '%s %f' -- "$1" && exit 0
[ -e "$1" ] && exit 1
exit 66`

// openScript prints the stat line, then the content of a regular file. A
// directory or another non-regular path gives only the stat line.
const openScript = `export LC_ALL=C
i=$(stat -L -c '%s %f' -- "$1") || { [ -e "$1" ] && exit 1; exit 66; }
if [ -f "$1" ]; then [ -r "$1" ] || exit 77; fi
printf '%s\n' "$i"
[ -f "$1" ] || exit 0
exec cat -- "$1"`

// prepareScript resolves a symlinked target, refuses what the user could not
// write, creates the missing parents 0755, and prints NUL-separated: created
// (1 or 0), the mode to keep, the resolved target and its directory.
const prepareScript = `export LC_ALL=C
umask 022
t=$1
if [ -L "$t" ]; then t=$(readlink -f -- "$t") && [ -n "$t" ] || exit 1; fi
[ -d "$t" ] && exit 21
d=${t%/*}; d=${d:-/}; b=$d
while [ ! -e "$b" ]; do b=${b%/*}; b=${b:-/}; done
[ -d "$b" ] || exit 20
[ -w "$b" ] && [ -x "$b" ] || exit 77
mkdir -p -- "$d" || exit 1
c=1 m=644
if [ -e "$t" ]; then [ -w "$t" ] || exit 77; c=0; m=$(stat -L -c %a -- "$t") || exit 1; fi
printf '%s\0%s\0%s\0%s\0' "$c" "$m" "$t" "$d"`

// streamScript writes stdin to the temporary file with the kept mode.
const streamScript = `umask 022; cat >"$1" && chmod -- "$2" "$1"`

// renameScript moves the temporary into place only when it holds exactly the
// expected size, against a stream that kubectl cut short.
const renameScript = `n=$(wc -c <"$1") && [ $n -eq "$3" ] && mv -f -- "$1" "$2" || { rm -f -- "$1"; exit 1; }`

// StatFile reports what is at a pod path, following a symlink. The container
// root is a directory here, as to a shell.
func (manager *Manager) StatFile(ctx context.Context, id, name string) (fs.FileInfo, error) {
	clean, err := filePath(name)
	if err != nil {
		return nil, err
	}
	result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", statScript, "aries-stat", clean}})
	if err != nil {
		return nil, fmt.Errorf("stat Kubernetes path: %w", err)
	}
	if result.ExitCode != 0 {
		return nil, fileFailure("stat Kubernetes path", result.ExitCode, result.Stderr)
	}
	stat, err := parseStat(result.Stdout)
	if err != nil {
		return nil, err
	}
	return fileInfo{name: path.Base(clean), stat: stat}, nil
}

// OpenFile streams one pod file, following a symlink, in one exec with no
// output bound. Closing the stream early stops kubectl. A non-regular path,
// the container root included, yields its info and an empty stream, and no
// cat starts.
func (manager *Manager) OpenFile(ctx context.Context, id, name string) (io.ReadCloser, fs.FileInfo, error) {
	clean, err := filePath(name)
	if err != nil {
		return nil, nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	go func() {
		var stderr bytes.Buffer
		err := manager.execInPod(streamCtx, id, nil, writer, &stderr, "/bin/sh", "-c", openScript, "aries-read", clean)
		if err != nil {
			err = execFailure(streamCtx, "read Kubernetes file", err, stderr.String())
		}
		_ = writer.CloseWithError(err)
	}()
	stream := &archiveStream{PipeReader: reader, cancel: cancel}
	content := bufio.NewReader(stream)
	line, err := content.ReadString('\n')
	if errors.Is(err, io.EOF) {
		err = errors.New("read Kubernetes file: no stat line")
	}
	if err != nil {
		_ = stream.Close()
		return nil, nil, err
	}
	stat, err := parseStat(line)
	if err != nil {
		_ = stream.Close()
		return nil, nil, err
	}
	info := fileInfo{name: path.Base(clean), stat: stat}
	if !stat.Mode.IsRegular() {
		_ = stream.Close()
		return io.NopCloser(strings.NewReader("")), info, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{content, stream}, info, nil
}

// WriteFile replaces or creates a pod file with exactly content, as Docker's
// WriteFile does: a symlink is followed, an existing file keeps its mode, a
// new one gets 0644, and parents are created 0755. The bytes land under a
// temporary name in the target's directory and one rename makes them visible.
// What the user could not write gives fs.ErrPermission. Parents created
// before a failed write remain.
func (manager *Manager) WriteFile(ctx context.Context, id, name string, content io.Reader, size int64, execUser string) (bool, error) {
	clean, err := filePath(name)
	if err != nil {
		return false, err
	}
	if clean == "/" {
		return false, syscall.EISDIR
	}
	var prepared, stderr bytes.Buffer
	result, err := manager.ExecStream(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", prepareScript, "aries-write", clean}, User: execUser}, nil, &prepared, &stderr)
	// An exec error stands alone, so a cancel stays a pure cancellation.
	if err != nil {
		return false, fmt.Errorf("prepare Kubernetes file: %w", err)
	}
	if result.ExitCode != 0 {
		return false, fileFailure("prepare Kubernetes file", result.ExitCode, stderr.String())
	}
	fields := strings.Split(prepared.String(), "\x00")
	if len(fields) != 5 || fields[4] != "" {
		return false, fmt.Errorf("prepare Kubernetes file: unexpected output %q", prepared.String())
	}
	created, mode, target, directory := fields[0] == "1", fields[1], fields[2], fields[3]

	token, err := randomID()
	if err != nil {
		return false, fmt.Errorf("generate Kubernetes write name: %w", err)
	}
	temporary := path.Join(directory, ".aries-tmp-"+token)
	streamReader, streamWriter := io.Pipe()
	streamErr := make(chan error, 1)
	go func() {
		written, writeErr := io.CopyN(streamWriter, content, size)
		if writeErr == nil && written != size {
			writeErr = io.ErrUnexpectedEOF
		}
		// The content must end exactly at size. Reading on also makes a
		// streamed caller finish before anything is renamed into place.
		if writeErr == nil {
			if _, endErr := io.ReadFull(content, make([]byte, 1)); endErr == nil {
				writeErr = errors.New("content is longer than its size")
			} else if !errors.Is(endErr, io.EOF) {
				writeErr = endErr
			}
		}
		_ = streamWriter.CloseWithError(writeErr)
		streamErr <- writeErr
	}()
	stderr.Reset()
	result, err = manager.ExecStream(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", streamScript, "aries-write", temporary, mode}, User: execUser}, streamReader, nil, &stderr)
	_ = streamReader.Close()
	copyErr := <-streamErr
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Cancelled during the stream or before the rename, e.g. by
		// revocation: the write must not land.
		manager.removeTemporary(id, temporary, execUser)
		return false, ctxErr
	}
	if err == nil && result.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", result.ExitCode, strings.TrimSpace(stderr.String()))
	}
	if err = errors.Join(err, copyErr); err != nil {
		manager.removeTemporary(id, temporary, execUser)
		return false, fmt.Errorf("write Kubernetes file: %w", err)
	}
	// A rename that started runs to its end on its own bounded context, so
	// the call reports whether the file landed.
	renameCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.options.CleanupTimeout)
	defer cancel()
	result, err = manager.Exec(renameCtx, id, core.Command{
		Path: "/bin/sh", User: execUser,
		Args: []string{"-c", renameScript, "aries-write", temporary, target, strconv.FormatInt(size, 10)},
	})
	if err != nil {
		manager.removeTemporary(id, temporary, execUser)
		return false, fmt.Errorf("rename Kubernetes file into place: %w", err)
	}
	if result.ExitCode != 0 {
		manager.removeTemporary(id, temporary, execUser)
		return false, fmt.Errorf("rename Kubernetes file into place: exit %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return created, nil
}

// removeTemporary deletes a write's temporary file with a fresh bounded
// context, since the caller's may already be cancelled.
func (manager *Manager) removeTemporary(id, name, execUser string) {
	ctx, cancel := context.WithTimeout(context.Background(), manager.options.CleanupTimeout)
	defer cancel()
	_, _ = manager.Exec(ctx, id, core.Command{Path: "rm", Args: []string{"-f", "--", name}, User: execUser})
}

// filePath cleans a target path. Unlike cleanContainerPath, it accepts the
// container root.
func filePath(name string) (string, error) {
	clean := path.Clean(name)
	if !strings.HasPrefix(clean, "/") || strings.ContainsRune(clean, 0) {
		return "", errors.New("path must be absolute and NUL-free")
	}
	return clean, nil
}

// fileFailure maps a script's exit code to the errors the target contract
// names. A missing path reports Permission denied only through stat's stderr.
func fileFailure(action string, code int, stderr string) error {
	switch code {
	case missingExit:
		if strings.Contains(stderr, "Permission denied") {
			return fmt.Errorf("%s: %w", action, fs.ErrPermission)
		}
		return fmt.Errorf("%s: %w", action, fs.ErrNotExist)
	case refusedExit:
		return fmt.Errorf("%s: %w", action, fs.ErrPermission)
	case isDirectoryExit:
		return fmt.Errorf("%s: %w", action, syscall.EISDIR)
	case notDirectoryExit:
		return fmt.Errorf("%s: %w", action, syscall.ENOTDIR)
	}
	return fmt.Errorf("%s: exit %d: %s", action, code, strings.TrimSpace(stderr))
}

// execFailure maps a failed kubectl run of a script. A cancel stays a pure
// cancellation.
func execFailure(ctx context.Context, action string, err error, stderr string) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() > 0 {
		return fileFailure(action, exit.ExitCode(), stderr)
	}
	return fmt.Errorf("%s: %w: %s", action, err, strings.TrimSpace(stderr))
}

// fileInfo adapts a pod stat to fs.FileInfo.
type fileInfo struct {
	name string
	stat deployment.FileInfo
}

func (f fileInfo) Name() string       { return f.name }
func (f fileInfo) Size() int64        { return f.stat.Size }
func (f fileInfo) Mode() fs.FileMode  { return f.stat.Mode }
func (f fileInfo) ModTime() time.Time { return time.Time{} }
func (f fileInfo) IsDir() bool        { return f.stat.Mode.IsDir() }
func (f fileInfo) Sys() any           { return nil }
