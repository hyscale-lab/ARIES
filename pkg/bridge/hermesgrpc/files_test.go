package hermesgrpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
	"github.com/hyscale-lab/aries/pkg/core"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// memorySandbox adds the file capability to testSandbox over an in-memory
// filesystem. Paths are absolute on the wire and unrooted in fstest.MapFS.
type memorySandbox struct {
	*testSandbox
	mu    sync.Mutex
	files fstest.MapFS
}

func newMemorySandbox(files fstest.MapFS) *memorySandbox {
	return &memorySandbox{testSandbox: &testSandbox{result: core.CommandResult{}}, files: files}
}

func (sandbox *memorySandbox) StatFile(_ context.Context, path string) (fs.FileInfo, error) {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	return fs.Stat(sandbox.files, strings.TrimPrefix(path, "/"))
}

func (sandbox *memorySandbox) OpenFile(_ context.Context, path string) (io.ReadCloser, fs.FileInfo, error) {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	file, err := sandbox.files.Open(strings.TrimPrefix(path, "/"))
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	return file, info, err
}

func (sandbox *memorySandbox) WriteFile(_ context.Context, path string, content io.Reader, _ int64) (bool, error) {
	data, err := io.ReadAll(content)
	if err != nil {
		return false, err
	}
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	name := strings.TrimPrefix(path, "/")
	mode := fs.FileMode(0o644)
	info, err := fs.Stat(sandbox.files, name)
	if err == nil && info.IsDir() {
		return false, syscall.EISDIR
	}
	if err == nil {
		mode = info.Mode()
	}
	sandbox.files[name] = &fstest.MapFile{Data: data, Mode: mode}
	return err != nil, nil
}

func startFileBridge(t *testing.T, sandbox any, options Options) (*Manager, sandboxv1.SandboxClient, core.ToolEndpoint) {
	t.Helper()
	options.OutputDir, options.ClientPath = t.TempDir(), fakeClient(t)
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	var endpoint core.ToolEndpoint
	switch value := sandbox.(type) {
	case *memorySandbox:
		endpoint, err = manager.Start(context.Background(), value)
	case *testSandbox:
		endpoint, err = manager.Start(context.Background(), value)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	client, closeClient := dial(t, endpoint)
	t.Cleanup(closeClient)
	return manager, client, endpoint
}

func TestFileProceduresMoveBytesThroughTheSandbox(t *testing.T) {
	sandbox := newMemorySandbox(fstest.MapFS{
		"app/src":     &fstest.MapFile{Mode: fs.ModeDir | 0o755},
		"app/old.bin": &fstest.MapFile{Data: []byte{0, 1, 2, 0xff}, Mode: 0o600},
	})
	_, client, _ := startFileBridge(t, sandbox, Options{})
	ctx := context.Background()

	written, err := client.WriteFile(ctx, &sandboxv1.WriteFileRequest{Path: "/app/new.txt", Content: []byte("one\ntwo\n")})
	if err != nil || written.GetBytesWritten() != 8 || !written.GetCreated() {
		t.Fatalf("create = %v, %v", written, err)
	}
	replaced, err := client.WriteFile(ctx, &sandboxv1.WriteFileRequest{Path: "/app/old.bin", Content: []byte("text")})
	if err != nil || replaced.GetCreated() {
		t.Fatalf("replace = %v, %v", replaced, err)
	}

	stat, err := client.Stat(ctx, &sandboxv1.StatRequest{Path: "/app/old.bin"})
	if err != nil || !stat.GetExists() || stat.GetType() != sandboxv1.FileType_FILE_TYPE_REGULAR || stat.GetSize() != 4 || stat.GetMode() != 0o600 {
		t.Fatalf("stat file = %v, %v (a replaced file must keep its mode)", stat, err)
	}
	if stat, err := client.Stat(ctx, &sandboxv1.StatRequest{Path: "/app/src"}); err != nil || stat.GetType() != sandboxv1.FileType_FILE_TYPE_DIRECTORY {
		t.Fatalf("stat directory = %v, %v", stat, err)
	}
	if stat, err := client.Stat(ctx, &sandboxv1.StatRequest{Path: "/app/missing"}); err != nil || stat.GetExists() {
		t.Fatalf("stat missing = %v, %v, want exists=false and no error", stat, err)
	}

	whole, err := client.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: "/app/new.txt"})
	if err != nil || string(whole.GetContent()) != "one\ntwo\n" || whole.GetSize() != 8 || whole.GetTruncated() {
		t.Fatalf("read whole = %v, %v", whole, err)
	}
	probe, err := client.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: "/app/new.txt", Offset: 1, MaxBytes: 3})
	if err != nil || string(probe.GetContent()) != "ne\nt"[:3] || probe.GetSize() != 8 || !probe.GetTruncated() {
		t.Fatalf("read range = %v, %v", probe, err)
	}
	lines, err := client.ReadLines(ctx, &sandboxv1.ReadLinesRequest{Path: "/app/new.txt", FirstLine: 2, MaxLines: 5})
	if err != nil || string(lines.GetContent()) != "two\n" || lines.GetTotalLines() != 2 || !lines.GetEndsWithNewline() || lines.GetMore() {
		t.Fatalf("read lines = %v, %v", lines, err)
	}
}

func TestFileProceduresMapFailuresToStatuses(t *testing.T) {
	sandbox := newMemorySandbox(fstest.MapFS{"app/dir": &fstest.MapFile{Mode: fs.ModeDir | 0o755}})
	_, client, _ := startFileBridge(t, sandbox, Options{OutputLimit: 16})
	ctx := context.Background()
	for name, test := range map[string]struct {
		call func() error
		want codes.Code
	}{
		"relative path": {func() error { _, err := client.Stat(ctx, &sandboxv1.StatRequest{Path: "app/dir"}); return err }, codes.InvalidArgument},
		"missing file": {func() error {
			_, err := client.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: "/app/none"})
			return err
		}, codes.NotFound},
		"read a directory": {func() error {
			_, err := client.ReadLines(ctx, &sandboxv1.ReadLinesRequest{Path: "/app/dir", FirstLine: 1, MaxLines: 1})
			return err
		}, codes.FailedPrecondition},
		"write over a directory": {func() error {
			_, err := client.WriteFile(ctx, &sandboxv1.WriteFileRequest{Path: "/app/dir", Content: []byte("x")})
			return err
		}, codes.FailedPrecondition},
		"content over the bound": {func() error {
			_, err := client.WriteFile(ctx, &sandboxv1.WriteFileRequest{Path: "/app/big", Content: make([]byte, 17)})
			return err
		}, codes.ResourceExhausted},
		"max_bytes over the bound": {func() error {
			_, err := client.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: "/app/none", MaxBytes: 17})
			return err
		}, codes.InvalidArgument},
		"empty line window": {func() error {
			_, err := client.ReadLines(ctx, &sandboxv1.ReadLinesRequest{Path: "/app/none", FirstLine: 1})
			return err
		}, codes.InvalidArgument},
	} {
		if got := status.Code(test.call()); got != test.want {
			t.Errorf("%s: status %v, want %v", name, got, test.want)
		}
	}
	if _, present := sandbox.files["app/big"]; present {
		t.Fatal("an over-bound write reached the sandbox")
	}
}

// A sandbox with no file capability is the first iteration's Docker sandbox:
// every call must still be recorded and answered UNIMPLEMENTED, never OK.
func TestFileCallsWithoutFileAccessAreRecordedAndUnimplemented(t *testing.T) {
	_, client, endpoint := startFileBridge(t, &testSandbox{}, Options{})
	_, err := client.WriteFile(context.Background(), &sandboxv1.WriteFileRequest{Path: "/app/x", Content: []byte("x")})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("write = %v, want Unimplemented", err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 || records[0]["operation_class"] != kindFileWrite || records[0]["status"] != "unimplemented" || records[0]["path"] != "/app/x" {
		t.Fatalf("records = %#v", records)
	}
}

func TestRevokedSessionRefusesFileCallsAsUnavailable(t *testing.T) {
	manager, _, _ := startFileBridge(t, newMemorySandbox(fstest.MapFS{}), Options{})
	session := manager.active
	session.revoke()
	svc := &service{session: session, serveCtx: context.Background()}
	if _, err := svc.Stat(context.Background(), &sandboxv1.StatRequest{Path: "/app"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("stat after revocation = %v, want Unavailable", err)
	}
}

// File content reaches the audit only when the profile asked for it.
func TestFileContentIsRetainedOnlyWhenAsked(t *testing.T) {
	for _, retain := range []bool{false, true} {
		sandbox := newMemorySandbox(fstest.MapFS{})
		manager, client, endpoint := startFileBridge(t, sandbox, Options{RetainContent: retain})
		if _, err := client.WriteFile(context.Background(), &sandboxv1.WriteFileRequest{Path: "/app/f", Content: []byte{0, 'x'}}); err != nil {
			t.Fatal(err)
		}
		if err := manager.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		records := readToolCalls(t, endpoint.LogPaths[0])
		raw, present := records[0]["content_raw"]
		if present != retain || retain && raw != base64.StdEncoding.EncodeToString([]byte{0, 'x'}) {
			t.Fatalf("retain=%v: record = %#v", retain, records[0])
		}
	}
}

// TestReadLinesMatchesTheShellPipeline uses the pipeline Hermes runs today as
// the oracle, so the typed procedure is a drop-in for it.
func TestReadLinesMatchesTheShellPipeline(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not available")
	}
	long := strings.Repeat("é", 3000) + "tail" // longer than bufio's buffer, multibyte
	for _, content := range []string{"", "a", "a\n", "a\nb", "one\ntwo\nthree\n", long + "\nshort\n" + long} {
		for _, window := range [][3]int64{{1, 2000, 0}, {2, 1, 0}, {1, 2, 5}, {3, 10, 7}, {9, 3, 0}} {
			first, count, clamp := window[0], window[1], window[2]
			got, err := readLines(strings.NewReader(content), first, count, clamp, 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			cut := ""
			if clamp > 0 {
				cut = " | cut -b1-" + strconv.FormatInt(clamp, 10)
			}
			script := "sed -n '" + strconv.FormatInt(first, 10) + "," + strconv.FormatInt(first+count-1, 10) + "p'" + cut
			want := shell(t, script, content)
			total, _ := strconv.ParseInt(strings.TrimSpace(shell(t, "wc -l", content)), 10, 64)
			// GNU cut terminates an unterminated final line; Hermes strips that
			// newline again after its `tail -c 1` check, and the procedure
			// returns the on-disk shape directly.
			if lastLine := total + 1; content != "" && !strings.HasSuffix(content, "\n") && first <= lastLine && lastLine <= first+count-1 {
				want = strings.TrimSuffix(want, "\n")
			}
			if string(got.GetContent()) != want || got.GetTotalLines() != total {
				t.Fatalf("content %q window %v:\ngot  %q (%d lines)\nwant %q (%d lines)", content[:min(len(content), 20)], window, got.GetContent(), got.GetTotalLines(), want, total)
			}
			if got.GetEndsWithNewline() != strings.HasSuffix(content, "\n") || got.GetMore() != (total > first+count-1) {
				t.Fatalf("content %q window %v: flags %v", content[:min(len(content), 20)], window, got)
			}
		}
	}
}

func shell(t *testing.T, script, input string) string {
	t.Helper()
	command := exec.Command("sh", "-c", script)
	command.Stdin = strings.NewReader(input)
	command.Env = []string{"LC_ALL=C"}
	var output bytes.Buffer
	command.Stdout = &output
	if err := command.Run(); err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return output.String()
}
