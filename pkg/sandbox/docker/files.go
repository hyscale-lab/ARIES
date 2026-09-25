package docker

// The file capability the Hermes gRPC bridge asserts on a sandbox: stat, read
// a file as a stream, and write one atomically. It uses the Docker archive
// API, which needs no binaries in the task image and no process per read.
//
// Known weakness, accepted for performance: the archive API acts as the
// daemon, i.e. root, not as the sandbox's exec user, so these methods can
// reach paths the agent's own shell cannot. docs/design/sandbox-rpc.md records
// it and the upgrade path.

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

	cerrdefs "github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// renameTimeout bounds the one exec that finishes a write, and its cleanup.
const renameTimeout = 30 * time.Second

// StatFile reports what is at a container path, following a symlink.
func (s *Sandbox) StatFile(ctx context.Context, name string) (fs.FileInfo, error) {
	clean, err := cleanFilePath(name)
	if err != nil {
		return nil, err
	}
	_, stat, err := s.resolvePath(ctx, clean)
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
func (s *Sandbox) OpenFile(ctx context.Context, name string) (io.ReadCloser, fs.FileInfo, error) {
	clean, err := cleanFilePath(name)
	if err != nil {
		return nil, nil, err
	}
	result, err := s.copyFrom(ctx, clean)
	if err == nil && result.Stat.Mode&os.ModeSymlink != 0 {
		_ = result.Content.Close()
		result, err = s.copyFrom(ctx, result.Stat.LinkTarget)
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
func (s *Sandbox) WriteFile(ctx context.Context, name string, content io.Reader, size int64) (bool, error) {
	clean, err := cleanFilePath(name)
	if err != nil {
		return false, err
	}
	target, stat, err := s.resolvePath(ctx, clean)
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
		resolved, stat, err := s.resolvePath(ctx, base)
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
	uid, gid := s.fileOwner()

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
		if closeErr := writer.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = archiveWriter.CloseWithError(writeErr)
		archiveErr <- writeErr
	}()
	_, copyErr := s.client.CopyToContainer(ctx, s.containerID, client.CopyToContainerOptions{
		DestinationPath: base, Content: archiveReader, CopyUIDGID: true,
	})
	_ = archiveReader.Close()
	if err := errors.Join(copyErr, <-archiveErr); err != nil {
		s.removeTemporary(temporary)
		return false, fmt.Errorf("write Docker file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		// Cancelled between the copy and the rename, e.g. by revocation: the
		// write must not land.
		s.removeTemporary(temporary)
		return false, err
	}
	result, err := s.Exec(ctx, core.Command{
		Path: "/bin/sh", User: rootExecUser, Timeout: renameTimeout,
		Args: []string{"-c", `mv -f -- "$1" "$2" || { rm -f -- "$1"; exit 1; }`, "aries-write", temporary, target},
	})
	if err != nil || result.ExitCode != 0 {
		s.removeTemporary(temporary)
		return false, fmt.Errorf("rename Docker file into place: %w", errors.Join(err, fmt.Errorf("exit %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))))
	}
	return created, nil
}

// removeTemporary deletes a write's temporary file with a fresh bounded
// context, since the caller's may already be cancelled.
func (s *Sandbox) removeTemporary(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), renameTimeout)
	defer cancel()
	_, _ = s.Exec(ctx, core.Command{Path: "/bin/rm", Args: []string{"-f", "--", name}, User: rootExecUser})
}

// resolvePath stats a path and, when it is a symlink, its target. Docker
// reports LinkTarget fully resolved and absolute, relative links included.
func (s *Sandbox) resolvePath(ctx context.Context, name string) (string, container.PathStat, error) {
	stat, err := s.statPath(ctx, name)
	if err == nil && stat.Mode&os.ModeSymlink != 0 {
		name = stat.LinkTarget
		stat, err = s.statPath(ctx, name)
	}
	return name, stat, err
}

func (s *Sandbox) statPath(ctx context.Context, name string) (container.PathStat, error) {
	result, err := s.client.ContainerStatPath(ctx, s.containerID, client.ContainerStatPathOptions{Path: name})
	if err != nil {
		return container.PathStat{}, s.pathError(ctx, "stat Docker path", err)
	}
	return result.Stat, nil
}

func (s *Sandbox) copyFrom(ctx context.Context, name string) (client.CopyFromContainerResult, error) {
	result, err := s.client.CopyFromContainer(ctx, s.containerID, client.CopyFromContainerOptions{SourcePath: name})
	if err != nil {
		return result, s.pathError(ctx, "read Docker file", err)
	}
	return result, nil
}

// pathError maps a daemon 404 to fs.ErrNotExist only when the container is
// still alive: the archive API's 404 is ambiguous between a missing path and
// a missing container, and a lost container is a failure, not an absence.
func (s *Sandbox) pathError(ctx context.Context, action string, err error) error {
	if s.missingPath(ctx, err) {
		return fmt.Errorf("%s: %w: %w", action, fs.ErrNotExist, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func (s *Sandbox) missingPath(ctx context.Context, err error) bool {
	if !cerrdefs.IsNotFound(err) {
		return false
	}
	_, inspectErr := s.client.ContainerInspect(ctx, s.containerID, client.ContainerInspectOptions{})
	return inspectErr == nil
}

// fileOwner is the exec user's numeric uid and gid, root when it is unset or
// not numeric. ARIES sets exec users only in numeric form.
func (s *Sandbox) fileOwner() (int, int) {
	uid, gid, ok := strings.Cut(s.execUser, ":")
	numericUID, uidErr := strconv.Atoi(uid)
	numericGID, gidErr := strconv.Atoi(gid)
	if !ok || uidErr != nil || gidErr != nil {
		return 0, 0
	}
	return numericUID, numericGID
}

func cleanFilePath(name string) (string, error) {
	if strings.ContainsRune(name, 0) || !strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("Docker file path %q must be absolute and NUL-free", name)
	}
	return cleanContainerPath(path.Clean(name))
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
