package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// fileDocker adds the archive API's path view to the exec fake.
type fileDocker struct {
	*fakeDocker
	stats       map[string]container.PathStat
	downloads   map[string]client.CopyFromContainerResult
	upload      client.CopyToContainerOptions
	uploadBytes []byte
	uploadErr   error
	onUpload    func()
	onExec      func(argv []string)
}

func newFileDocker() (*fileDocker, *Manager) {
	fake := &fileDocker{fakeDocker: newFakeDocker()}
	fake.container.ID = "container"
	fake.container.State = &container.State{Running: true}
	return fake, &Manager{client: fake}
}

func (fake *fileDocker) ContainerStatPath(_ context.Context, id string, options client.ContainerStatPathOptions) (client.ContainerStatPathResult, error) {
	stat, ok := fake.stats[options.Path]
	if !ok {
		return client.ContainerStatPathResult{}, fmt.Errorf("stat %s in %s: %w", options.Path, id, errdefs.ErrNotFound)
	}
	return client.ContainerStatPathResult{Stat: stat}, nil
}

func (fake *fileDocker) CopyFromContainer(_ context.Context, id string, options client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	result, ok := fake.downloads[options.SourcePath]
	if !ok {
		return result, fmt.Errorf("copy %s from %s: %w", options.SourcePath, id, errdefs.ErrNotFound)
	}
	return result, nil
}

func (fake *fileDocker) CopyToContainer(_ context.Context, _ string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	fake.upload = options
	if fake.onUpload != nil {
		fake.onUpload()
	}
	content, err := io.ReadAll(options.Content)
	if fake.uploadErr != nil {
		return client.CopyToContainerResult{}, fake.uploadErr
	}
	if err != nil {
		return client.CopyToContainerResult{}, err
	}
	fake.uploadBytes = content
	return client.CopyToContainerResult{}, nil
}

// ExecCreate refuses a done context, as the daemon client does, so a cleanup
// on the caller's cancelled context records no exec.
func (fake *fileDocker) ExecCreate(ctx context.Context, id string, options client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if fake.onExec != nil {
		fake.onExec(commandArgs(options.Cmd))
	}
	if err := ctx.Err(); err != nil {
		return client.ExecCreateResult{}, err
	}
	return fake.fakeDocker.ExecCreate(ctx, id, options)
}

// execCommands lists each exec's argv in order, without the exec wrapper.
func (fake *fileDocker) execCommands() [][]string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var commands [][]string
	for index := 1; index <= fake.nextExec; index++ {
		commands = append(commands, commandArgs(fake.execs[fmt.Sprintf("exec-%d", index)].Cmd))
	}
	return commands
}

func directory(mode fs.FileMode) container.PathStat {
	return container.PathStat{Mode: fs.ModeDir | mode}
}

func regular(size int64, mode fs.FileMode) container.PathStat {
	return container.PathStat{Size: size, Mode: mode}
}

func fileArchive(t *testing.T, name string, content []byte) io.ReadCloser {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return io.NopCloser(&buffer)
}

// uploadedEntries decodes what WriteFile sent to CopyToContainer.
func uploadedEntries(t *testing.T, fake *fileDocker) []*tar.Header {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(fake.uploadBytes))
	var headers []*tar.Header
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return headers
		}
		if err != nil {
			t.Fatal(err)
		}
		headers = append(headers, header)
	}
}

// commandArgs strips the exec wrapper, leaving the command's own argv. The
// wrapper's last element is the command path.
func commandArgs(cmd []string) []string {
	return cmd[len(wrappedCommand("", "", core.Command{}))-1:]
}

func TestStatAndOpenFollowSymlinksAndMapAbsence(t *testing.T) {
	fake, manager := newFileDocker()
	fake.stats = map[string]container.PathStat{
		"/work/link":     {Mode: os.ModeSymlink | 0o777, LinkTarget: "/work/real.txt"},
		"/work/real.txt": regular(5, 0o600),
	}
	fake.downloads = map[string]client.CopyFromContainerResult{
		"/work/link":     {Content: io.NopCloser(strings.NewReader("")), Stat: fake.stats["/work/link"]},
		"/work/real.txt": {Content: fileArchive(t, "real.txt", []byte("hello")), Stat: fake.stats["/work/real.txt"]},
	}

	info, err := manager.StatFile(context.Background(), "container", "/work/link")
	if err != nil || !info.Mode().IsRegular() || info.Size() != 5 {
		t.Fatalf("StatFile(link) = %v, %v", info, err)
	}
	stream, info, err := manager.OpenFile(context.Background(), "container", "/work/link")
	if err != nil || info.Size() != 5 {
		t.Fatalf("OpenFile(link) = %v, %v", info, err)
	}
	content, _ := io.ReadAll(stream)
	_ = stream.Close()
	if string(content) != "hello" {
		t.Fatalf("content = %q", content)
	}
	if _, err := manager.StatFile(context.Background(), "container", "/work/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing path = %v, want fs.ErrNotExist", err)
	}
	// A 404 from a container that is gone is a failure, not an absence.
	fake.mu.Lock()
	fake.removed = true
	fake.mu.Unlock()
	if _, _, err := manager.OpenFile(context.Background(), "container", "/work/missing"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lost container = %v, want a non-absence failure", err)
	}
}

// WriteFile mirrors Hermes's _atomic_write: `mkdir -p` for missing parents
// only, the kept mode on replace, 0644 on create, the exec user as owner, and
// a temporary name renamed into place. The parents, the access check and the
// rename are execs with exact argv; the archive carries only the temporary.
func TestWriteFileStagesATemporaryAndRenamesIt(t *testing.T) {
	fake, manager := newFileDocker()
	fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o700)}

	created, err := manager.WriteFile(context.Background(), "container", "/work/a/b/f.txt", strings.NewReader("data"), 4, "65532:65532")
	if err != nil || !created {
		t.Fatalf("WriteFile = %v, %v", created, err)
	}
	// The exec user owns the file through the header, not the image USER.
	if fake.upload.DestinationPath != "/work/a/b" || fake.upload.CopyUIDGID {
		t.Fatalf("upload = %+v", fake.upload)
	}
	headers := uploadedEntries(t, fake)
	if len(headers) != 1 {
		t.Fatalf("entries = %v", headers)
	}
	file := headers[0]
	if !strings.HasPrefix(file.Name, ".aries-tmp-") || file.Typeflag != tar.TypeReg || file.Mode != 0o644 || file.Size != 4 || file.Uid != 65532 || file.Gid != 65532 {
		t.Fatalf("file entry = %+v", file)
	}
	commands := fake.execCommands()
	access := []string{"/bin/sh", "-c", `umask 022; [ -w "$1" ] && [ -x "$1" ] || exit 77; mkdir -p -- "$2" || exit 1; [ ! -e "$3" ] || [ -w "$3" ] || exit 77`, "aries-write", "/work", "/work/a/b", "/work/a/b/f.txt"}
	rename := []string{"/bin/sh", "-c", `mv -f -- "$1" "$2" || { rm -f -- "$1"; exit 1; }`, "aries-write", "/work/a/b/" + file.Name, "/work/a/b/f.txt"}
	if !reflect.DeepEqual(commands, [][]string{access, rename}) {
		t.Fatalf("execs = %q", commands)
	}

	// Replacing keeps the existing mode, special bits included. Without an
	// exec user, the daemon gives the file to the image USER.
	fake.stats["/work/a"], fake.stats["/work/a/b"] = directory(0o755), directory(0o755)
	fake.stats["/work/a/b/f.txt"] = regular(4, 0o750|fs.ModeSetuid)
	created, err = manager.WriteFile(context.Background(), "container", "/work/a/b/f.txt", strings.NewReader("new"), 3, "")
	if err != nil || created {
		t.Fatalf("replace = %v, %v", created, err)
	}
	headers = uploadedEntries(t, fake)
	if len(headers) != 1 || headers[0].Mode != 0o4750 || headers[0].Uid != 0 || headers[0].Gid != 0 || fake.upload.DestinationPath != "/work/a/b" || !fake.upload.CopyUIDGID {
		t.Fatalf("replace entries = %+v to %+v", headers, fake.upload)
	}
}

// No exec of a typed write runs as root when the exec user is set: not the
// access check, not the rename, not the cleanup of a failed copy.
func TestWriteFileExecsRunAsTheExecUser(t *testing.T) {
	for _, uploadErr := range []error{nil, errors.New("daemon went away")} {
		fake, manager := newFileDocker()
		fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o755)}
		fake.uploadErr = uploadErr

		_, err := manager.WriteFile(context.Background(), "container", "/work/f", strings.NewReader("x"), 1, "65532:65532")
		if (err != nil) != (uploadErr != nil) {
			t.Fatalf("WriteFile = %v with upload error %v", err, uploadErr)
		}
		if fake.nextExec == 0 {
			t.Fatal("no exec ran")
		}
		for execID, options := range fake.execs {
			if options.User != "65532:65532" {
				t.Fatalf("%s %q ran as %q", execID, commandArgs(options.Cmd), options.User)
			}
		}
	}
}

func TestWriteFileRefusesDirectoriesWithoutWriting(t *testing.T) {
	fake, manager := newFileDocker()
	fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o755), "/work/file": regular(1, 0o644)}

	if _, err := manager.WriteFile(context.Background(), "container", "/work", strings.NewReader(""), 0, ""); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("write over a directory = %v, want EISDIR", err)
	}
	if _, err := manager.WriteFile(context.Background(), "container", "/work/file/child", strings.NewReader(""), 0, ""); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("write under a file = %v, want ENOTDIR", err)
	}
	if fake.uploadBytes != nil || len(fake.execCommands()) != 0 {
		t.Fatal("a refused write reached the container")
	}
}

// A failed copy must never rename: the temporary is removed and the original
// stays as it was. The caller's context is cancelled during the copy, so the
// cleanup must use its own.
func TestWriteFileCleansUpAFailedCopy(t *testing.T) {
	fake, manager := newFileDocker()
	fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o755)}
	fake.uploadErr = errors.New("daemon went away")
	ctx, cancel := context.WithCancel(context.Background())
	fake.onUpload = cancel

	if _, err := manager.WriteFile(ctx, "container", "/work/f", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("a failed copy was reported as written")
	}
	commands := fake.execCommands()
	if len(commands) != 2 {
		t.Fatalf("execs = %q, want the access check and the cleanup", commands)
	}
	cleanup := commands[1]
	if cleanup[0] != "/bin/rm" || !strings.HasPrefix(cleanup[3], "/work/.aries-tmp-") {
		t.Fatalf("cleanup = %q", cleanup)
	}
}

// Content that runs past or stops short of its declared size is a malformed
// body: the archive fails, the temporary is removed, and nothing is renamed
// into place.
func TestWriteFileRefusesContentOfAnotherSize(t *testing.T) {
	for _, content := range []string{"longer", "sh"} {
		t.Run(content, func(t *testing.T) {
			fake, manager := newFileDocker()
			fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o755)}

			if _, err := manager.WriteFile(context.Background(), "container", "/work/f", strings.NewReader(content), 3, ""); err == nil {
				t.Fatal("content of another size was written")
			}
			for _, command := range fake.execCommands() {
				if len(command) > 2 && strings.HasPrefix(command[2], "mv ") {
					t.Fatalf("exec %q renamed a malformed body into place", command)
				}
			}
		})
	}
}

// pureCancellation follows bridgekit.IsPureCancellation, which this package
// cannot import: every leaf of the error tree is a context error.
func pureCancellation(err error) bool {
	switch e := err.(type) {
	case nil:
		return false
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			if !pureCancellation(child) {
				return false
			}
		}
		return len(e.Unwrap()) > 0
	case interface{ Unwrap() error }:
		return pureCancellation(e.Unwrap())
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

// A cancel during a write gives a pure cancellation, so revocation can tell it
// from a sandbox failure: at the access check, during the copy, and at the
// rename. Nothing is renamed into place.
func TestWriteFileCancelIsAPureCancellation(t *testing.T) {
	for _, stage := range []string{"access", "copy", "rename"} {
		t.Run(stage, func(t *testing.T) {
			fake, manager := newFileDocker()
			fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o755)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "access":
				cancel()
			case "copy":
				// The archive goroutine sees the reader closed under it.
				fake.uploadErr, fake.onUpload = io.ErrClosedPipe, cancel
			case "rename":
				fake.onExec = func(argv []string) {
					if strings.HasPrefix(argv[2], "mv ") {
						cancel()
					}
				}
			}

			if _, err := manager.WriteFile(ctx, "container", "/work/f", strings.NewReader("x"), 1, "65532:65532"); !pureCancellation(err) {
				t.Fatalf("WriteFile = %v, want a pure cancellation", err)
			}
		})
	}
}
