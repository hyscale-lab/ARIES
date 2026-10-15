//go:build integration

package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/sirupsen/logrus"
)

const fixtureImage = "docker.io/library/busybox:1.37.0-musl@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94"

// TestFileCapabilityAgainstARealContainer drives StatFile, OpenFile and
// WriteFile against a real container, checking the postconditions Hermes's
// own _atomic_write provides.
func TestFileCapabilityAgainstARealContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := PullImages(ctx, "", []string{fixtureImage}); err != nil {
		t.Fatalf("Docker daemon and fixture image are required for integration tests: %v", err)
	}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	manager, err := New(Options{CleanupTimeout: 20 * time.Second, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	id, err := manager.Create(ctx, deployment.Request{
		Image: fixtureImage, Workdir: "/work", Args: []string{"sleep", "3600"}, Init: true,
		Placement: core.RuntimePlacement{AttachmentID: "none"},
	})
	t.Cleanup(func() { _ = manager.Stop(context.Background(), id) })
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	const execUser = "65532:65532"
	shell := func(script string) string {
		t.Helper()
		result, err := manager.Exec(ctx, id, core.Command{Path: "/bin/sh", Args: []string{"-c", script}, User: rootExecUser})
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("%s: exit %d: %s: %v", script, result.ExitCode, result.Stderr, err)
		}
		return strings.TrimSpace(result.Stdout)
	}
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
		created, err := manager.WriteFile(ctx, id, path, bytes.NewReader(content), int64(len(content)), execUser)
		if err != nil {
			t.Fatalf("WriteFile(%s) = %v", path, err)
		}
		return created
	}
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
