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
// a temporary name renamed into place by one exec with exact argv.
func TestWriteFileStagesATemporaryAndRenamesIt(t *testing.T) {
	fake, manager := newFileDocker()
	fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o700)}

	created, err := manager.WriteFile(context.Background(), "container", "/work/a/b/f.txt", strings.NewReader("data"), 4, "65532:65532")
	if err != nil || !created {
		t.Fatalf("WriteFile = %v, %v", created, err)
	}
	if fake.upload.DestinationPath != "/work" || !fake.upload.CopyUIDGID {
		t.Fatalf("upload = %+v", fake.upload)
	}
	headers := uploadedEntries(t, fake)
	if len(headers) != 3 || headers[0].Name != "a/" || headers[1].Name != "a/b/" {
		t.Fatalf("entries = %v", headers)
	}
	for _, header := range headers[:2] {
		if header.Typeflag != tar.TypeDir || header.Mode != 0o755 || header.Uid != 65532 || header.Gid != 65532 {
			t.Fatalf("directory entry = %+v", header)
		}
	}
	file := headers[2]
	if !strings.HasPrefix(file.Name, "a/b/.aries-tmp-") || file.Mode != 0o644 || file.Size != 4 || file.Uid != 65532 {
		t.Fatalf("file entry = %+v", file)
	}
	commands := fake.execCommands()
	rename := commands[len(commands)-1]
	want := []string{"/bin/sh", "-c", `mv -f -- "$1" "$2" || { rm -f -- "$1"; exit 1; }`, "aries-write", "/work/" + file.Name, "/work/a/b/f.txt"}
	if !reflect.DeepEqual(rename, want) || fake.execs[fmt.Sprintf("exec-%d", fake.nextExec)].User != rootExecUser {
		t.Fatalf("rename = %q", rename)
	}

	// Replacing keeps the existing mode, special bits included, and adds no
	// directory entries. Without an exec user, root owns the file.
	fake.stats["/work/a"], fake.stats["/work/a/b"] = directory(0o755), directory(0o755)
	fake.stats["/work/a/b/f.txt"] = regular(4, 0o750|fs.ModeSetuid)
	created, err = manager.WriteFile(context.Background(), "container", "/work/a/b/f.txt", strings.NewReader("new"), 3, "")
	if err != nil || created {
		t.Fatalf("replace = %v, %v", created, err)
	}
	headers = uploadedEntries(t, fake)
	if len(headers) != 1 || headers[0].Mode != 0o4750 || headers[0].Uid != 0 || headers[0].Gid != 0 || fake.upload.DestinationPath != "/work/a/b" {
		t.Fatalf("replace entries = %+v to %s", headers, fake.upload.DestinationPath)
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
// stays as it was. The caller's context is already cancelled, so the cleanup
// must use its own.
func TestWriteFileCleansUpAFailedCopy(t *testing.T) {
	fake, manager := newFileDocker()
	fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o755)}
	fake.uploadErr = errors.New("daemon went away")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := manager.WriteFile(ctx, "container", "/work/f", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("a failed copy was reported as written")
	}
	commands := fake.execCommands()
	if len(commands) != 1 {
		t.Fatalf("execs = %q, want only the cleanup", commands)
	}
	cleanup := commands[0]
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
				if command[0] != "/bin/rm" {
					t.Fatalf("exec %q ran; only the cleanup may", command)
				}
			}
		})
	}
}
