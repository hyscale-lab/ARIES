package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

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
func uploadedEntries(t *testing.T, fake *fakeClient) []*tar.Header {
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

// commandArgs strips the exec wrapper, leaving the command's own argv.
func commandArgs(cmd []string) []string {
	return cmd[6:]
}

func TestStatAndOpenFollowSymlinksAndMapAbsence(t *testing.T) {
	fake := &fakeClient{}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	fake.stats = map[string]container.PathStat{
		"/work/link":     {Mode: os.ModeSymlink | 0o777, LinkTarget: "/work/real.txt"},
		"/work/real.txt": regular(5, 0o600),
	}
	fake.downloads = map[string]client.CopyFromContainerResult{
		"/work/link":     {Content: io.NopCloser(strings.NewReader("")), Stat: fake.stats["/work/link"]},
		"/work/real.txt": {Content: fileArchive(t, "real.txt", []byte("hello")), Stat: fake.stats["/work/real.txt"]},
	}

	info, err := sandbox.StatFile(context.Background(), "/work/link")
	if err != nil || !info.Mode().IsRegular() || info.Size() != 5 {
		t.Fatalf("StatFile(link) = %v, %v", info, err)
	}
	stream, info, err := sandbox.OpenFile(context.Background(), "/work/link")
	if err != nil || info.Size() != 5 {
		t.Fatalf("OpenFile(link) = %v, %v", info, err)
	}
	content, _ := io.ReadAll(stream)
	_ = stream.Close()
	if string(content) != "hello" {
		t.Fatalf("content = %q", content)
	}
	if _, err := sandbox.StatFile(context.Background(), "/work/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing path = %v, want fs.ErrNotExist", err)
	}
	// A 404 from a container that is gone is a failure, not an absence.
	fake.mu.Lock()
	fake.containerID = ""
	fake.mu.Unlock()
	if _, _, err := sandbox.OpenFile(context.Background(), "/work/missing"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lost container = %v, want a non-absence failure", err)
	}
}

// WriteFile mirrors Hermes's _atomic_write: `mkdir -p` for missing parents
// only, the kept mode on replace, 0644 on create, the exec user as owner, and
// a temporary name renamed into place by one exec with exact argv.
func TestWriteFileStagesATemporaryAndRenamesIt(t *testing.T) {
	fake := &fakeClient{}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	sandbox.execUser = "65532:65532"
	fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o700)}

	created, err := sandbox.WriteFile(context.Background(), "/work/a/b/f.txt", strings.NewReader("data"), 4)
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
	rename := commandArgs(fake.execCommands[len(fake.execCommands)-1])
	want := []string{"/bin/sh", "-c", `mv -f -- "$1" "$2" || { rm -f -- "$1"; exit 1; }`, "aries-write", "/work/" + file.Name, "/work/a/b/f.txt"}
	if !reflect.DeepEqual(rename, want) || fake.execOptions.User != rootExecUser {
		t.Fatalf("rename = %q as %q", rename, fake.execOptions.User)
	}

	// Replacing keeps the existing mode, special bits included, and adds no
	// directory entries.
	fake.stats["/work/a"], fake.stats["/work/a/b"] = directory(0o755), directory(0o755)
	fake.stats["/work/a/b/f.txt"] = regular(4, 0o750|fs.ModeSetuid)
	created, err = sandbox.WriteFile(context.Background(), "/work/a/b/f.txt", strings.NewReader("new"), 3)
	if err != nil || created {
		t.Fatalf("replace = %v, %v", created, err)
	}
	headers = uploadedEntries(t, fake)
	if len(headers) != 1 || headers[0].Mode != 0o4750 || fake.upload.DestinationPath != "/work/a/b" {
		t.Fatalf("replace entries = %+v to %s", headers, fake.upload.DestinationPath)
	}
}

func TestWriteFileRefusesDirectoriesWithoutWriting(t *testing.T) {
	fake := &fakeClient{}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o755), "/work/file": regular(1, 0o644)}

	if _, err := sandbox.WriteFile(context.Background(), "/work", strings.NewReader(""), 0); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("write over a directory = %v, want EISDIR", err)
	}
	if _, err := sandbox.WriteFile(context.Background(), "/work/file/child", strings.NewReader(""), 0); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("write under a file = %v, want ENOTDIR", err)
	}
	if fake.uploadBytes != nil || len(fake.execCommands) != 0 {
		t.Fatal("a refused write reached the container")
	}
}

// A failed copy must never rename: the temporary is removed and the original
// stays as it was.
func TestWriteFileCleansUpAFailedCopy(t *testing.T) {
	fake := &fakeClient{}
	sandbox := startSandbox(t, fake)
	defer sandbox.stop(context.Background())
	fake.stats = map[string]container.PathStat{"/": directory(0o755), "/work": directory(0o755)}
	fake.uploadErr = errors.New("daemon went away")
	fake.execCommands = nil

	if _, err := sandbox.WriteFile(context.Background(), "/work/f", strings.NewReader("x"), 1); err == nil {
		t.Fatal("a failed copy was reported as written")
	}
	if len(fake.execCommands) != 1 {
		t.Fatalf("execs = %q, want only the cleanup", fake.execCommands)
	}
	cleanup := commandArgs(fake.execCommands[0])
	if cleanup[0] != "/bin/rm" || !strings.HasPrefix(cleanup[3], "/work/.aries-tmp-") {
		t.Fatalf("cleanup = %q", cleanup)
	}
}
