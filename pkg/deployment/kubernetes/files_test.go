package kubernetes

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testPod = "aries-sandbox-abc123"

// hostFiles runs each exec's remote argv on the host, so the real scripts meet
// a real filesystem under t.TempDir(). It drops the setpriv prefix: the test
// process stands in for the exec user. Every cat the scripts start is counted
// in the returned file.
func hostFiles(t *testing.T) (*Manager, *fakeKubectl, string) {
	t.Helper()
	manager, fake := newTestManager(t, Options{})
	bin := t.TempDir()
	cats := filepath.Join(t.TempDir(), "cats")
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\necho cat >> '" + cats + "'\nexec '" + catPath + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "cat"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	fake.respond = func(ctx context.Context, args []string, stdin []byte, stdout, stderr io.Writer) error {
		remote := args[slices.Index(args, "--")+1:]
		if slices.Contains(remote, "setpriv") {
			remote = remote[slices.Index(remote, "--")+1:]
		}
		command := exec.CommandContext(ctx, remote[0], remote[1:]...)
		command.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		command.Stdin, command.Stdout, command.Stderr = strings.NewReader(string(stdin)), stdout, stderr
		err := command.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitError(exit.ExitCode())
		}
		return err
	}
	return manager, fake, cats
}

func catCount(t *testing.T, cats string) int {
	t.Helper()
	content, err := os.ReadFile(cats)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(content), "cat\n")
}

func writeHostFile(t *testing.T, name, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}

func TestStatFileFollowsSymlinksAndMapsAbsence(t *testing.T) {
	manager, _, _ := hostFiles(t)
	root := t.TempDir()
	writeHostFile(t, filepath.Join(root, "file"), "hello", 0o640)
	for _, link := range [][2]string{{"file", "link"}, {"none", "dangling"}} {
		if err := os.Symlink(filepath.Join(root, link[0]), filepath.Join(root, link[1])); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"file", "link"} {
		info, err := manager.StatFile(context.Background(), testPod, filepath.Join(root, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() != 5 || info.Mode().Perm() != 0o640 {
			t.Fatalf("StatFile(%s) = %v, %v", name, info, err)
		}
	}
	for _, name := range []string{root, "/"} {
		if info, err := manager.StatFile(context.Background(), testPod, name); err != nil || !info.IsDir() {
			t.Fatalf("StatFile(%s) = %v, %v, want a directory", name, info, err)
		}
	}
	for _, name := range []string{"none", "dangling", "file/child"} {
		if _, err := manager.StatFile(context.Background(), testPod, filepath.Join(root, name)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("StatFile(%s) = %v, want fs.ErrNotExist", name, err)
		}
	}
	if _, err := manager.StatFile(context.Background(), testPod, "relative"); err == nil {
		t.Fatal("relative path accepted")
	}
}

// A lost pod makes kubectl fail with its own exit status, which is a
// failure, not an absence.
func TestStatFileOfALostPodIsAFailure(t *testing.T) {
	manager, fake := newTestManager(t, Options{})
	fake.respond = func(_ context.Context, _ []string, _ []byte, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "Error from server (NotFound): pods \"aries-sandbox-abc123\" not found\n")
		return exitError(1)
	}
	if _, err := manager.StatFile(context.Background(), testPod, "/app/f"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lost pod = %v, want a non-absence failure", err)
	}
}

func TestOpenFileStreamsRegularFilesOnly(t *testing.T) {
	manager, _, cats := hostFiles(t)
	root := t.TempDir()
	content := "a\x00b\nline two\n\xff"
	writeHostFile(t, filepath.Join(root, "file"), content, 0o600)
	if err := os.Symlink(filepath.Join(root, "file"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	stream, info, err := manager.OpenFile(context.Background(), testPod, filepath.Join(root, "link"))
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(content)) {
		t.Fatalf("OpenFile(link) = %v, %v", info, err)
	}
	got, err := io.ReadAll(stream)
	if err != nil || string(got) != content || stream.Close() != nil {
		t.Fatalf("content = %q, %v", got, err)
	}
	reads := catCount(t, cats)

	for _, name := range []string{root, "/"} {
		stream, info, err := manager.OpenFile(context.Background(), testPod, name)
		if err != nil || !info.IsDir() {
			t.Fatalf("OpenFile(%s) = %v, %v, want a directory", name, info, err)
		}
		if got, err := io.ReadAll(stream); err != nil || len(got) != 0 {
			t.Fatalf("OpenFile(%s) stream = %q, %v, want empty", name, got, err)
		}
		_ = stream.Close()
	}
	if catCount(t, cats) != reads {
		t.Fatal("a directory was read with cat")
	}
	if _, _, err := manager.OpenFile(context.Background(), testPod, filepath.Join(root, "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenFile(missing) = %v, want fs.ErrNotExist", err)
	}
}

func TestWriteFileCreatesReplacesAndFollowsSymlinks(t *testing.T) {
	manager, _, _ := hostFiles(t)
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "f.bin")

	created, err := manager.WriteFile(context.Background(), testPod, target, strings.NewReader("data"), 4, "")
	if err != nil || !created {
		t.Fatalf("WriteFile(new) = %v, %v", created, err)
	}
	for name, want := range map[string]fs.FileMode{filepath.Join(root, "a"): fs.ModeDir | 0o755, filepath.Join(root, "a", "b"): fs.ModeDir | 0o755, target: 0o644} {
		if info, err := os.Stat(name); err != nil || info.Mode() != want {
			t.Fatalf("%s = %v, %v, want mode %v", name, info, err, want)
		}
	}

	// Replacing keeps the mode, special bits included, and renames a new
	// inode into place.
	if err := os.Chmod(target, 0o750|fs.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(target)
	created, err = manager.WriteFile(context.Background(), testPod, target, strings.NewReader("new"), 3, "")
	after, statErr := os.Stat(target)
	if err != nil || created || statErr != nil || after.Mode() != 0o750|fs.ModeSetuid || os.SameFile(before, after) {
		t.Fatalf("replace = %v, %v; mode %v", created, err, after.Mode())
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.WriteFile(context.Background(), testPod, link, strings.NewReader("through"), 7, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); string(got) != "through" {
		t.Fatalf("symlink write landed elsewhere: %q", got)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	assertNoTemporary(t, filepath.Join(root, "a", "b"))
}

// A write through a dangling symlink creates the file the link names, and
// the link stays a link.
func TestWriteFileThroughADanglingSymlinkCreatesItsTarget(t *testing.T) {
	manager, _, _ := hostFiles(t)
	root := t.TempDir()
	target, link := filepath.Join(root, "new.txt"), filepath.Join(root, "dangling")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	created, err := manager.WriteFile(context.Background(), testPod, link, strings.NewReader("hi"), 2, "")
	if err != nil || !created {
		t.Fatalf("WriteFile(dangling) = %v, %v", created, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "hi" {
		t.Fatalf("target = %q", got)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	assertNoTemporary(t, root)
}

// Closing a read before its end stops kubectl and returns cleanly. The file
// is larger than a pipe buffer, so cat is still writing at the close.
func TestOpenFileCloseBeforeTheEndStopsKubectl(t *testing.T) {
	const pipeOverflow = 4 << 20
	manager, fake, _ := hostFiles(t)
	run := fake.respond
	stopped := make(chan error, 1)
	fake.respond = func(ctx context.Context, args []string, stdin []byte, stdout, stderr io.Writer) error {
		err := run(ctx, args, stdin, stdout, stderr)
		stopped <- ctx.Err()
		return err
	}
	name := filepath.Join(t.TempDir(), "large")
	writeHostFile(t, name, strings.Repeat("x", pipeOverflow), 0o644)

	stream, _, err := manager.OpenFile(context.Background(), testPod, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(stream, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("kubectl ended with context %v, want canceled", err)
		}
	case <-time.After(manager.options.CleanupTimeout):
		t.Fatal("kubectl still runs after Close")
	}
}

func TestWriteFileRefusesWithoutWriting(t *testing.T) {
	manager, fake, _ := hostFiles(t)
	root := t.TempDir()
	writeHostFile(t, filepath.Join(root, "file"), "original", 0o644)

	for _, name := range []string{root, "/"} {
		if _, err := manager.WriteFile(context.Background(), testPod, name, strings.NewReader("x"), 1, ""); !errors.Is(err, syscall.EISDIR) {
			t.Fatalf("WriteFile(%s) = %v, want EISDIR", name, err)
		}
	}
	if _, err := manager.WriteFile(context.Background(), testPod, filepath.Join(root, "file", "child"), strings.NewReader("x"), 1, ""); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("write under a file = %v, want ENOTDIR", err)
	}
	calls := len(fake.calls)
	for _, content := range []string{"longer", "sh"} {
		if _, err := manager.WriteFile(context.Background(), testPod, filepath.Join(root, "file"), strings.NewReader(content), 3, ""); err == nil {
			t.Fatalf("content %q of another size was written", content)
		}
	}
	if len(fake.calls) == calls {
		t.Fatal("the size refusals ran no exec")
	}
	if got, _ := os.ReadFile(filepath.Join(root, "file")); string(got) != "original" {
		t.Fatalf("a refused write changed the file: %q", got)
	}
	assertNoTemporary(t, root)
}

// A read-only parent refuses the write with fs.ErrPermission. Root bypasses
// the mode bits, so the case needs an unprivileged test user.
func TestWriteFileRefusesAReadOnlyParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	manager, _, _ := hostFiles(t)
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	for _, name := range []string{filepath.Join(locked, "new"), filepath.Join(locked, "x", "y")} {
		if _, err := manager.WriteFile(context.Background(), testPod, name, strings.NewReader("x"), 1, ""); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("WriteFile(%s) = %v, want fs.ErrPermission", name, err)
		}
	}
	if entries, _ := os.ReadDir(locked); len(entries) != 0 {
		t.Fatalf("a refused write left %v", entries)
	}
}

// A cancel before the rename never lands the write and removes the
// temporary. A rename that started runs to its end and the write succeeds.
func TestWriteFileCancelBeforeTheRenameNeverLands(t *testing.T) {
	for _, stage := range []string{"stream", "rename"} {
		t.Run(stage, func(t *testing.T) {
			manager, fake, _ := hostFiles(t)
			run := fake.respond
			root := t.TempDir()
			target := filepath.Join(root, "f")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			script := map[string]string{"stream": streamScript, "rename": renameScript}[stage]
			fake.respond = func(callCtx context.Context, args []string, stdin []byte, stdout, stderr io.Writer) error {
				if !slices.Contains(args, script) {
					return run(callCtx, args, stdin, stdout, stderr)
				}
				// The stream lands the temporary before the cancel; the
				// rename starts after it.
				if stage == "stream" {
					defer cancel()
					return run(callCtx, args, stdin, stdout, stderr)
				}
				cancel()
				return run(callCtx, args, stdin, stdout, stderr)
			}

			created, err := manager.WriteFile(ctx, testPod, target, strings.NewReader("x"), 1, "")
			_, statErr := os.Stat(target)
			if stage == "rename" {
				if err != nil || !created || statErr != nil {
					t.Fatalf("started rename = %v, %v; file %v", created, err, statErr)
				}
				return
			}
			if !errors.Is(err, context.Canceled) || !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("cancelled write = %v; file %v", err, statErr)
			}
			assertNoTemporary(t, root)
		})
	}
}

// Writes act as the exec user through setpriv; reads act as the container
// user, so they carry no setpriv prefix.
func TestWritesUseSetprivAndReadsDoNot(t *testing.T) {
	manager, fake, _ := hostFiles(t)
	root := t.TempDir()
	target := filepath.Join(root, "f")
	if _, err := manager.WriteFile(context.Background(), testPod, target, strings.NewReader("x"), 1, "65532:65532"); err != nil {
		t.Fatal(err)
	}
	writes := len(fake.calls)
	if _, err := manager.StatFile(context.Background(), testPod, target); err != nil {
		t.Fatal(err)
	}
	stream, _, err := manager.OpenFile(context.Background(), testPod, target)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(stream)
	_ = stream.Close()
	for index, call := range fake.calls {
		remote := call[slices.Index(call, "--")+1:]
		user := slices.Equal(remote[:min(len(remote), 5)], []string{"env", "HOME=/", "setpriv", "--reuid=65532", "--regid=65532"})
		if user != (index < writes) {
			t.Fatalf("call %d %q: setpriv %v, want %v", index, remote, user, index < writes)
		}
	}
}

func assertNoTemporary(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".aries-tmp-") {
			t.Fatalf("temporary file left behind: %s", entry.Name())
		}
	}
}
