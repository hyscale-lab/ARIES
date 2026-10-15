package docker

// The file capability a borrowed bridge target asserts on a runtime: stat,
// read a file as a stream, and write one atomically. It uses the Docker
// archive API, which needs no binaries in the task image and no process per
// read.
//
// Known weakness, accepted for performance: the archive API acts as the
// daemon, i.e. root, not as the runtime's exec user, so these methods can
// reach paths the agent's own shell cannot. It is accepted because verifier
// material arrives only after revocation. docs/design/bridge.md records it.

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// StatFile reports what is at a container path, following a symlink.
func (manager *Manager) StatFile(ctx context.Context, id, name string) (fs.FileInfo, error) {
	clean, err := cleanContainerPath(path.Clean(name))
	if err != nil {
		return nil, err
	}
	_, stat, err := manager.resolvePath(ctx, id, clean)
	if err != nil {
		return nil, err
	}
	return pathInfo{stat}, nil
}

// OpenFile streams one container file, following a symlink. Closing the
// stream early stops the transfer, so a short probe reads only what it needs.
// A non-regular path yields its info and an empty stream.
//
// ponytail: tmpfs mounts (e.g. /dev/shm) are invisible to the archive API and
// read as not found; add an exec `cat` fallback if an agent ever needs them.
func (manager *Manager) OpenFile(ctx context.Context, id, name string) (io.ReadCloser, fs.FileInfo, error) {
	clean, err := cleanContainerPath(path.Clean(name))
	if err != nil {
		return nil, nil, err
	}
	result, err := manager.copyFrom(ctx, id, clean)
	if err == nil && result.Stat.Mode&os.ModeSymlink != 0 {
		_ = result.Content.Close()
		result, err = manager.copyFrom(ctx, id, result.Stat.LinkTarget)
	}
	if err != nil {
		return nil, nil, err
	}
	info := pathInfo{result.Stat}
	if !result.Stat.Mode.IsRegular() {
		_ = result.Content.Close()
		return io.NopCloser(strings.NewReader("")), info, nil
	}
	archive := tar.NewReader(result.Content)
	if _, err := archive.Next(); err != nil {
		_ = result.Content.Close()
		return nil, nil, fmt.Errorf("read Docker file archive: %w", err)
	}
	return struct {
		io.Reader
		io.Closer
	}{archive, result.Content}, info, nil
}

// WriteFile replaces or creates a container file with exactly content, the
// way Hermes's own _atomic_write does: parents are created as `mkdir -p`
// would (0755, owned by the exec user, existing ones untouched), a symlink is
// followed, an existing file keeps its mode, a new one gets 0644. The bytes
// land under a temporary name in the target's directory and one rename makes
// them visible, because Docker's own extraction deletes the old file and
// writes in place: a reader sees the old file or the new one, and a failed
// write leaves the original intact.
func (manager *Manager) WriteFile(ctx context.Context, id, name string, content io.Reader, size int64, execUser string) (bool, error) {
	clean, err := cleanContainerPath(path.Clean(name))
	if err != nil {
		return false, err
	}
	target, stat, err := manager.resolvePath(ctx, id, clean)
	created := errors.Is(err, fs.ErrNotExist)
	if err != nil && !created {
		return false, err
	}
	mode := int64(0o644)
	if !created {
		if stat.Mode.IsDir() {
			return false, syscall.EISDIR
		}
		mode = tarMode(stat.Mode)
	}

	// Walk up to the nearest existing directory; everything below it is
	// created by the archive, and nothing at or above it is touched.
	base, missing := path.Dir(target), []string(nil)
	for {
		resolved, stat, err := manager.resolvePath(ctx, id, base)
		if err == nil {
			if !stat.Mode.IsDir() {
				return false, syscall.ENOTDIR
			}
			base = resolved
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		missing = append([]string{path.Base(base)}, missing...)
		base = path.Dir(base)
	}

	token, err := randomID()
	if err != nil {
		return false, fmt.Errorf("generate Docker write name: %w", err)
	}
	directory := path.Join(append([]string{base}, missing...)...)
	temporary := path.Join(directory, ".aries-tmp-"+token)
	uid, gid := fileOwner(execUser)

	archiveReader, archiveWriter := io.Pipe()
	archiveErr := make(chan error, 1)
	go func() {
		writer := tar.NewWriter(archiveWriter)
		var writeErr error
		for index := range missing {
			writeErr = writer.WriteHeader(&tar.Header{
				Name: path.Join(missing[:index+1]...) + "/", Typeflag: tar.TypeDir, Mode: 0o755,
				Uid: uid, Gid: gid, ModTime: time.Now(),
			})
			if writeErr != nil {
				break
			}
		}
		if writeErr == nil {
			writeErr = writer.WriteHeader(&tar.Header{
				Name: path.Join(append(append([]string(nil), missing...), path.Base(temporary))...), Typeflag: tar.TypeReg,
				Mode: mode, Size: size, Uid: uid, Gid: gid, ModTime: time.Now(),
			})
		}
		if writeErr == nil {
			var written int64
			written, writeErr = io.CopyN(writer, content, size)
			if writeErr == nil && written != size {
				writeErr = io.ErrUnexpectedEOF
			}
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
		if closeErr := writer.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = archiveWriter.CloseWithError(writeErr)
		archiveErr <- writeErr
	}()
	_, copyErr := manager.client.CopyToContainer(ctx, id, client.CopyToContainerOptions{
		DestinationPath: base, Content: archiveReader, CopyUIDGID: true,
	})
	_ = archiveReader.Close()
	if err := errors.Join(copyErr, <-archiveErr); err != nil {
		manager.removeTemporary(id, temporary)
		return false, fmt.Errorf("write Docker file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		// Cancelled between the copy and the rename, e.g. by revocation: the
		// write must not land.
		manager.removeTemporary(id, temporary)
		return false, err
	}
	result, err := manager.Exec(ctx, id, core.Command{
		Path: "/bin/sh", User: rootExecUser, Timeout: manager.cleanupTimeout(),
		Args: []string{"-c", `mv -f -- "$1" "$2" || { rm -f -- "$1"; exit 1; }`, "aries-write", temporary, target},
	})
	if err != nil || result.ExitCode != 0 {
		manager.removeTemporary(id, temporary)
		return false, fmt.Errorf("rename Docker file into place: %w", errors.Join(err, fmt.Errorf("exit %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))))
	}
	return created, nil
}

// removeTemporary deletes a write's temporary file with a fresh bounded
// context, since the caller's may already be cancelled.
func (manager *Manager) removeTemporary(id, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), manager.cleanupTimeout())
	defer cancel()
	_, _ = manager.Exec(ctx, id, core.Command{Path: "/bin/rm", Args: []string{"-f", "--", name}, User: rootExecUser})
}

// resolvePath stats a path and, when it is a symlink, its target. Docker
// reports LinkTarget fully resolved and absolute, relative links included.
func (manager *Manager) resolvePath(ctx context.Context, id, name string) (string, container.PathStat, error) {
	stat, err := manager.statPath(ctx, id, name)
	if err == nil && stat.Mode&os.ModeSymlink != 0 {
		name = stat.LinkTarget
		stat, err = manager.statPath(ctx, id, name)
	}
	return name, stat, err
}

func (manager *Manager) statPath(ctx context.Context, id, name string) (container.PathStat, error) {
	result, err := manager.client.ContainerStatPath(ctx, id, client.ContainerStatPathOptions{Path: name})
	if err != nil {
		return container.PathStat{}, manager.pathError(ctx, id, "stat Docker path", err)
	}
	return result.Stat, nil
}

func (manager *Manager) copyFrom(ctx context.Context, id, name string) (client.CopyFromContainerResult, error) {
	result, err := manager.client.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: name})
	if err != nil {
		return result, manager.pathError(ctx, id, "read Docker file", err)
	}
	return result, nil
}

// pathError maps a daemon 404 to fs.ErrNotExist only when the container is
// still alive: the archive API's 404 is ambiguous between a missing path and
// a missing container, and a lost container is a failure, not an absence.
func (manager *Manager) pathError(ctx context.Context, id, action string, err error) error {
	if manager.missingPath(ctx, id, err) {
		return fmt.Errorf("%s: %w: %w", action, fs.ErrNotExist, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func (manager *Manager) missingPath(ctx context.Context, id string, err error) bool {
	if !errdefs.IsNotFound(err) {
		return false
	}
	_, inspectErr := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	return inspectErr == nil
}

// fileOwner is the exec user's numeric uid and gid, root when it is unset or
// not numeric. ARIES sets exec users only in numeric form.
func fileOwner(execUser string) (int, int) {
	uid, gid, ok := strings.Cut(execUser, ":")
	numericUID, uidErr := strconv.Atoi(uid)
	numericGID, gidErr := strconv.Atoi(gid)
	if !ok || uidErr != nil || gidErr != nil {
		return 0, 0
	}
	return numericUID, numericGID
}

// tarMode carries permission and special bits the way `stat -c%a` + `chmod`
// preserve them.
func tarMode(mode fs.FileMode) int64 {
	bits := int64(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

// pathInfo adapts the daemon's stat to fs.FileInfo.
type pathInfo struct{ stat container.PathStat }

func (p pathInfo) Name() string       { return p.stat.Name }
func (p pathInfo) Size() int64        { return p.stat.Size }
func (p pathInfo) Mode() fs.FileMode  { return p.stat.Mode }
func (p pathInfo) ModTime() time.Time { return p.stat.Mtime }
func (p pathInfo) IsDir() bool        { return p.stat.Mode.IsDir() }
func (p pathInfo) Sys() any           { return nil }
