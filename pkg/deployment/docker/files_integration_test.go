//go:build integration

package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/sirupsen/logrus"
)

const (
	fixtureImage = "docker.io/library/busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94"
	// userImage is the fixture with a non-root image USER, built locally.
	userImage = "aries-test/busybox-user:1"
	imageUser = "1234:1234"
	agentUser = "65532:65532"
	rootUser  = "0:0"
)

// fileContainer starts a sleeping container from image, or skips when Docker
// or the fixture image is unavailable.
func fileContainer(t *testing.T, ctx context.Context, image string) (*Manager, string) {
	t.Helper()
	if err := PullImages(ctx, "", []string{fixtureImage}); err != nil {
		t.Skipf("Docker daemon and fixture image are required for integration tests: %v", err)
	}
	if err := BuildImage(ctx, "", userImage, "FROM "+fixtureImage+"\nUSER "+imageUser+"\n", nil, nil); err != nil {
		t.Fatal(err)
	}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	manager, err := New(Options{CleanupTimeout: 20 * time.Second, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	id, err := manager.Create(ctx, deployment.Request{
		Image: image, Workdir: "/work", Args: []string{"sleep", "3600"}, Init: true,
		Placement: core.RuntimePlacement{AttachmentID: "none"},
	})
	t.Cleanup(func() { _ = manager.Stop(context.Background(), id) })
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	return manager, id
}

func rootShell(t *testing.T, ctx context.Context, manager *Manager, id, script string) string {
	t.Helper()
	result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", script}, User: rootUser})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("%s: exit %d: %s: %v", script, result.ExitCode, result.Stderr, err)
	}
	return strings.TrimSpace(result.Stdout)
}

// TestFileCapabilityAgainstARealContainer drives StatFile, OpenFile and
// WriteFile against a real container, checking the postconditions Hermes's
// own _atomic_write provides.
func TestFileCapabilityAgainstARealContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	manager, id := fileContainer(t, ctx, fixtureImage)
	shell := func(script string) string { t.Helper(); return rootShell(t, ctx, manager, id, script) }
	read := func(path string) []byte {
		t.Helper()
		stream, _, err := manager.OpenFile(ctx, id, path)
		if err != nil {
			t.Fatalf("OpenFile(%s) = %v", path, err)
		}
		defer stream.Close()
		content, err := io.ReadAll(stream)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	write := func(path string, content []byte) bool {
		t.Helper()
		created, err := manager.WriteFile(ctx, id, path, bytes.NewReader(content), int64(len(content)), agentUser)
		if err != nil {
			t.Fatalf("WriteFile(%s) = %v", path, err)
		}
		return created
	}
	// The agent owns its workdir; typed writes act as the agent.
	shell("chown 65532:65532 /work")
	before := shell("stat -c '%u %a' /work")

	// Create through missing parents: they appear as `mkdir -p` would make
	// them, owned by the exec user; the existing parent is untouched.
	binary := []byte("a\x00b\nline two\n\xff")
	if !write("/work/a/b/f.bin", binary) {
		t.Fatal("a new file was not reported as created")
	}
	if got := read("/work/a/b/f.bin"); !bytes.Equal(got, binary) {
		t.Fatalf("read back %q", got)
	}
	if got := shell("stat -c '%u %a' /work/a /work/a/b /work/a/b/f.bin | tr '\\n' ,"); got != "65532 755,65532 755,65532 644," {
		t.Fatalf("created ownership and modes = %q", got)
	}
	if got := shell("stat -c '%u %a' /work"); got != before {
		t.Fatalf("existing parent changed from %q to %q", before, got)
	}

	// Replace: the mode is kept and the inode changes, so the new bytes were
	// renamed into place rather than written over the old file.
	shell("chmod 640 /work/a/b/f.bin")
	inode := shell("stat -c %i /work/a/b/f.bin")
	if write("/work/a/b/f.bin", []byte("replaced")) {
		t.Fatal("a replaced file was reported as created")
	}
	if got := shell("stat -c '%a' /work/a/b/f.bin"); got != "640" {
		t.Fatalf("replaced mode = %s", got)
	}
	if shell("stat -c %i /work/a/b/f.bin") == inode {
		t.Fatal("the replace rewrote the file in place instead of renaming")
	}
	if got := shell("ls -A /work/a/b"); got != "f.bin" {
		t.Fatalf("temporary files left behind: %q", got)
	}

	// A symlink is followed for both read and write, and stays a symlink.
	shell("ln -s /work/a/b/f.bin /work/link")
	write("/work/link", []byte("through the link"))
	if got := string(read("/work/link")); got != "through the link" || shell("cat /work/a/b/f.bin") != "through the link" {
		t.Fatalf("symlink write landed elsewhere: %q", got)
	}
	if shell("test -L /work/link && echo link") != "link" {
		t.Fatal("the symlink was replaced by a file")
	}

	// A relative symlink resolves against its own directory.
	shell("ln -s b/f.bin /work/a/rel")
	if got := string(read("/work/a/rel")); got != "through the link" {
		t.Fatalf("relative symlink read %q", got)
	}

	// 70 MiB streams both ways through the archive API: above the 16 MiB exec
	// output cap, so no exec carries file content.
	large := bytes.Repeat([]byte("0123456789abcdef"), 70<<20/16+7)
	write("/work/large.bin", large)
	if got := read("/work/large.bin"); !bytes.Equal(got, large) {
		t.Fatalf("large file round trip: %d bytes, want %d", len(got), len(large))
	}

	info, err := manager.StatFile(ctx, id, "/work/a")
	if err != nil || !info.IsDir() {
		t.Fatalf("StatFile(dir) = %v, %v", info, err)
	}
	if _, err := manager.StatFile(ctx, id, "/work/none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("StatFile(missing) = %v", err)
	}
}

// A typed write acts as the user of the agent's shell: the exec user, or the
// image USER when none is set. What that user cannot write is refused with
// fs.ErrPermission, and nothing is left behind.
func TestWriteFileRefusesWhatTheExecUserCannotWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, tc := range []struct{ image, execUser string }{{fixtureImage, agentUser}, {userImage, ""}} {
		manager, id := fileContainer(t, ctx, tc.image)
		rootShell(t, ctx, manager, id, "mkdir -m 777 /shared && echo original > /shared/root.txt && chmod 644 /shared/root.txt")

		for _, name := range []string{"/bin/new", "/bin/x/y/new", "/shared/root.txt"} {
			if _, err := manager.WriteFile(ctx, id, name, strings.NewReader("x"), 1, tc.execUser); !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("%s: WriteFile(%s) as %q = %v, want fs.ErrPermission", tc.image, name, tc.execUser, err)
			}
		}
		if got := rootShell(t, ctx, manager, id, "ls -A /bin /shared | grep -E '^(new|x|[.]aries-tmp-.*)$'; cat /shared/root.txt"); got != "original" {
			t.Fatalf("%s: a refused write changed the container: %q", tc.image, got)
		}
	}
}

// New files and parents belong to the exec user, or to the image USER when
// no exec user is set, the way the agent's own `mkdir -p` would make them.
func TestWriteFileOwnerFollowsTheExecUser(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, tc := range []struct{ image, execUser, owner string }{
		{fixtureImage, "", "0:0"},
		{userImage, "", imageUser},
		{userImage, agentUser, agentUser},
	} {
		manager, id := fileContainer(t, ctx, tc.image)
		if _, err := manager.WriteFile(ctx, id, "/tmp/d1/d2/f.txt", strings.NewReader("hi"), 2, tc.execUser); err != nil {
			t.Fatalf("%s as %q: %v", tc.image, tc.execUser, err)
		}
		want := strings.Repeat(tc.owner+",", 3)
		if got := rootShell(t, ctx, manager, id, "stat -c '%u:%g' /tmp/d1 /tmp/d1/d2 /tmp/d1/d2/f.txt | tr '\\n' ,"); got != want {
			t.Fatalf("%s as %q: owners %q, want %q", tc.image, tc.execUser, got, want)
		}
	}
}

// The edge cases of the typed file contract, as the daemon answers them: a
// path under a file and a dangling symlink do not exist, and the root and
// any directory give their info and an empty stream to OpenFile.
func TestFileEdgeCasesAgainstARealContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	manager, id := fileContainer(t, ctx, fixtureImage)
	rootShell(t, ctx, manager, id, "echo data > /work/file && ln -s /work/none /work/dangling && mkdir /work/dir")

	for _, name := range []string{"/work/file/child", "/work/dangling", "/work/none"} {
		if _, err := manager.StatFile(ctx, id, name); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("StatFile(%s) = %v, want fs.ErrNotExist", name, err)
		}
	}
	for _, name := range []string{"/", "/work/dir"} {
		stream, info, err := manager.OpenFile(ctx, id, name)
		if err != nil || !info.IsDir() {
			t.Fatalf("OpenFile(%s) = %v, %v, want a directory", name, info, err)
		}
		content, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil || len(content) != 0 {
			t.Fatalf("OpenFile(%s) stream = %d bytes, %v, want empty", name, len(content), err)
		}
	}
	for _, name := range []string{"/", "/work/dir"} {
		if _, err := manager.WriteFile(ctx, id, name, strings.NewReader("x"), 1, agentUser); !errors.Is(err, syscall.EISDIR) {
			t.Fatalf("WriteFile(%s) = %v, want EISDIR", name, err)
		}
	}
	if _, err := manager.WriteFile(ctx, id, "/work/file/child", strings.NewReader("x"), 1, agentUser); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("WriteFile(/work/file/child) = %v, want ENOTDIR", err)
	}
	for _, content := range []string{"longer", "sh"} {
		if _, err := manager.WriteFile(ctx, id, "/work/file", strings.NewReader(content), 3, ""); err == nil {
			t.Fatalf("content %q of another size was written", content)
		}
	}
	if got := rootShell(t, ctx, manager, id, "cat /work/file; ls -A /work | grep -c '^[.]aries-tmp-' || true"); got != "data\n0" {
		t.Fatalf("a refused write changed the container: %q", got)
	}
}
