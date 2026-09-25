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
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

// TestFileCapabilityAgainstARealContainer drives StatFile, OpenFile and
// WriteFile against a real container, checking the postconditions Hermes's
// own _atomic_write provides.
func TestFileCapabilityAgainstARealContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Docker daemon is required for integration tests: %v", err)
	}
	ensureFixtureImage(t, ctx, api)
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	manager, err := New(Options{OutputDir: t.TempDir(), CleanupTimeout: 20 * time.Second, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	live, err := manager.Start(ctx, core.SandboxRequest{
		RunID: "files-run", TaskID: "files-task",
		Environment: core.Environment{Image: fixtureImage, Workdir: "/work", CPU: 0.5, MemoryMB: 64},
	})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*Sandbox)
	t.Cleanup(func() { _ = manager.Stop(context.Background(), live) })
	// Set after start rather than in the request: an exec user turns on
	// no-new-privileges, and some hosts cannot start docker-init under it.
	// Ownership reads only this field.
	sandbox.execUser = "65532:65532"
	shell := func(script string) string {
		t.Helper()
		result := execForTest(t, ctx, sandbox, core.Command{Path: "/bin/sh", Args: []string{"-c", script}, User: rootExecUser})
		if result.ExitCode != 0 {
			t.Fatalf("%s: exit %d: %s", script, result.ExitCode, result.Stderr)
		}
		return strings.TrimSpace(result.Stdout)
	}
	read := func(path string) []byte {
		t.Helper()
		stream, _, err := sandbox.OpenFile(ctx, path)
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
		created, err := sandbox.WriteFile(ctx, path, bytes.NewReader(content), int64(len(content)))
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

	// More than the 16 MiB exec output cap streams through the archive API.
	large := bytes.Repeat([]byte("0123456789abcdef"), 1<<20+7)
	write("/work/large.bin", large)
	if got := read("/work/large.bin"); !bytes.Equal(got, large) {
		t.Fatalf("large file round trip: %d bytes, want %d", len(got), len(large))
	}

	info, err := sandbox.StatFile(ctx, "/work/a")
	if err != nil || !info.IsDir() {
		t.Fatalf("StatFile(dir) = %v, %v", info, err)
	}
	if _, err := sandbox.StatFile(ctx, "/work/none"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("StatFile(missing) = %v", err)
	}
}
