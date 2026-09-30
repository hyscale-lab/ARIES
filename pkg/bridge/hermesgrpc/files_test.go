package hermesgrpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// WriteFile holds the whole body before it touches the file, and reads to
// io.EOF as the Docker sandbox does, so a malformed stream changes nothing.
func (sandbox *memorySandbox) WriteFile(_ context.Context, path string, content io.Reader, size int64) (bool, error) {
	data, err := io.ReadAll(content)
	if err != nil {
		return false, err
	}
	if int64(len(data)) != size {
		return false, io.ErrUnexpectedEOF
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

// writeFile streams data after a header declaring size, as the client does.
// A size different from len(data) makes a malformed stream on purpose.
func writeFile(client sandboxv1.SandboxClient, path string, size int64, data []byte) (*sandboxv1.WriteFileResponse, error) {
	stream, err := client.WriteFile(context.Background())
	if err != nil {
		return nil, err
	}
	err = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Header{Header: &sandboxv1.WriteFileHeader{Path: path, Size: size}}})
	for len(data) > 0 && err == nil {
		chunk := data[:min(len(data), chunkBytes)]
		data = data[len(chunk):]
		err = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Chunk{Chunk: chunk}})
	}
	return stream.CloseAndRecv()
}

// readFile collects a ReadFile stream: the concatenated chunks and the header.
func readFile(client sandboxv1.SandboxClient, request *sandboxv1.ReadFileRequest) ([]byte, *sandboxv1.ReadFileHeader, error) {
	stream, err := client.ReadFile(context.Background(), request)
	if err != nil {
		return nil, nil, err
	}
	var content []byte
	var header *sandboxv1.ReadFileHeader
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			return content, header, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if message.GetHeader() != nil {
			header = message.GetHeader()
		}
		content = append(content, message.GetChunk()...)
	}
}

// readWindow collects a ReadLines stream: the concatenated chunks and the
// summary.
func readWindow(client sandboxv1.SandboxClient, request *sandboxv1.ReadLinesRequest) ([]byte, *sandboxv1.ReadLinesSummary, error) {
	stream, err := client.ReadLines(context.Background(), request)
	if err != nil {
		return nil, nil, err
	}
	var content []byte
	var summary *sandboxv1.ReadLinesSummary
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			return content, summary, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if message.GetSummary() != nil {
			summary = message.GetSummary()
		}
		content = append(content, message.GetChunk()...)
	}
}

func TestFileProceduresMoveBytesThroughTheSandbox(t *testing.T) {
	sandbox := newMemorySandbox(fstest.MapFS{
		"app/src":     &fstest.MapFile{Mode: fs.ModeDir | 0o755},
		"app/old.bin": &fstest.MapFile{Data: []byte{0, 1, 2, 0xff}, Mode: 0o600},
	})
	_, client, _ := startFileBridge(t, sandbox, Options{})
	ctx := context.Background()

	written, err := writeFile(client, "/app/new.txt", 8, []byte("one\ntwo\n"))
	if err != nil || written.GetBytesWritten() != 8 || !written.GetCreated() {
		t.Fatalf("create = %v, %v", written, err)
	}
	replaced, err := writeFile(client, "/app/old.bin", 4, []byte("text"))
	if err != nil || replaced.GetCreated() {
		t.Fatalf("replace = %v, %v", replaced, err)
	}
	if empty, err := writeFile(client, "/app/empty", 0, nil); err != nil || !empty.GetCreated() || len(sandbox.files["app/empty"].Data) != 0 {
		t.Fatalf("empty write = %v, %v", empty, err)
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

	whole, header, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/new.txt"})
	if err != nil || string(whole) != "one\ntwo\n" || header.GetSize() != 8 || header.GetTruncated() {
		t.Fatalf("read whole = %q %v, %v", whole, header, err)
	}
	probe, header, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/new.txt", Offset: 1, MaxBytes: 3})
	if err != nil || string(probe) != "ne\n" || header.GetSize() != 8 || !header.GetTruncated() {
		t.Fatalf("read range = %q %v, %v", probe, header, err)
	}
	lines, summary, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/new.txt", FirstLine: 2, MaxLines: 5})
	if err != nil || string(lines) != "two\n" || summary.GetTotalLines() != 2 || summary.GetSize() != 8 || !summary.GetEndsWithNewline() || summary.GetMore() {
		t.Fatalf("read lines = %q %v, %v", lines, summary, err)
	}
}

// File content has no bound: 20 MiB is over the old 16 MiB file bound and
// grpc-go's 4 MiB default message size, and crosses in chunks both ways.
func TestFileContentLargerThanOneMessageStreams(t *testing.T) {
	sandbox := newMemorySandbox(fstest.MapFS{})
	_, client, _ := startFileBridge(t, sandbox, Options{})
	line := []byte(strings.Repeat("x", 99) + "\n")
	body := bytes.Repeat(line, 20<<20/len(line)+1)

	if _, err := writeFile(client, "/app/big", int64(len(body)), body); err != nil {
		t.Fatal(err)
	}
	got, header, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/big"})
	if err != nil || !bytes.Equal(got, body) || header.GetSize() != int64(len(body)) {
		t.Fatalf("read back %d bytes (header %v), want %d: %v", len(got), header, len(body), err)
	}
	window, summary, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/big", FirstLine: 1, MaxLines: 1 << 30})
	if err != nil || !bytes.Equal(window, body) || summary.GetTotalLines() != int64(len(body)/len(line)) || summary.GetMore() {
		t.Fatalf("window of %d bytes (summary %v), want the whole file: %v", len(window), summary, err)
	}
}

func TestFileProceduresMapFailuresToStatuses(t *testing.T) {
	sandbox := newMemorySandbox(fstest.MapFS{
		"app/dir":  &fstest.MapFile{Mode: fs.ModeDir | 0o755},
		"app/kept": &fstest.MapFile{Data: []byte("old"), Mode: 0o644},
	})
	_, client, _ := startFileBridge(t, sandbox, Options{})
	ctx := context.Background()
	for name, test := range map[string]struct {
		call func() error
		want codes.Code
	}{
		"relative path": {func() error { _, err := client.Stat(ctx, &sandboxv1.StatRequest{Path: "app/dir"}); return err }, codes.InvalidArgument},
		"missing file": {func() error {
			_, _, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/none"})
			return err
		}, codes.NotFound},
		"read a directory": {func() error {
			_, _, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/dir", FirstLine: 1, MaxLines: 1})
			return err
		}, codes.FailedPrecondition},
		"write over a directory":       {func() error { _, err := writeFile(client, "/app/dir", 1, []byte("x")); return err }, codes.FailedPrecondition},
		"stream shorter than its size": {func() error { _, err := writeFile(client, "/app/kept", 5, []byte("new")); return err }, codes.InvalidArgument},
		"stream longer than its size":  {func() error { _, err := writeFile(client, "/app/kept", 2, []byte("new")); return err }, codes.InvalidArgument},
		"negative max_bytes": {func() error {
			_, _, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/none", MaxBytes: -1})
			return err
		}, codes.InvalidArgument},
		"empty line window": {func() error {
			_, _, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/none", FirstLine: 1})
			return err
		}, codes.InvalidArgument},
	} {
		if got := status.Code(test.call()); got != test.want {
			t.Errorf("%s: status %v, want %v", name, got, test.want)
		}
	}
	if kept := string(sandbox.files["app/kept"].Data); kept != "old" {
		t.Fatalf("a malformed write stream changed the file to %q", kept)
	}
}

// A sandbox with no file capability is the first iteration's Docker sandbox:
// every call must still be recorded and answered UNIMPLEMENTED, never OK.
func TestFileCallsWithoutFileAccessAreRecordedAndUnimplemented(t *testing.T) {
	_, client, endpoint := startFileBridge(t, &testSandbox{}, Options{})
	if _, err := writeFile(client, "/app/x", 1, []byte("x")); status.Code(err) != codes.Unimplemented {
		t.Fatalf("write = %v, want Unimplemented", err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 || records[0]["operation_class"] != kindFileWrite || records[0]["status"] != "unimplemented" || records[0]["path"] != "/app/x" {
		t.Fatalf("records = %#v", records)
	}
}

func TestRevokedSessionRefusesFileCallsAsUnavailable(t *testing.T) {
	manager, _, _ := startFileBridge(t, newMemorySandbox(fstest.MapFS{}), Options{})
	session := manager.slot.Active().(*bridgeSession)
	session.revoke()
	svc := &service{session: session, serveCtx: context.Background()}
	if _, err := svc.Stat(context.Background(), &sandboxv1.StatRequest{Path: "/app"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("stat after revocation = %v, want Unavailable", err)
	}
}

// Every completed file record carries the content's sha256; the content
// itself reaches the audit only when the profile asked for it.
func TestFileContentIsRetainedOnlyWhenAsked(t *testing.T) {
	content := []byte{0, 'x'}
	digest := sha256.Sum256(content)
	for _, retain := range []bool{false, true} {
		sandbox := newMemorySandbox(fstest.MapFS{})
		manager, client, endpoint := startFileBridge(t, sandbox, Options{RetainContent: retain})
		if _, err := writeFile(client, "/app/f", 2, content); err != nil {
			t.Fatal(err)
		}
		if err := manager.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		records := readToolCalls(t, endpoint.LogPaths[0])
		raw, present := records[0]["content_raw"]
		if present != retain || retain && raw != base64.StdEncoding.EncodeToString(content) || records[0]["sha256"] != hex.EncodeToString(digest[:]) {
			t.Fatalf("retain=%v: record = %#v", retain, records[0])
		}
	}
}

// Retained file content is exempt from the audit's size limit, which still
// bounds every other record: a large retained write leaves the audit intact,
// while a retained stdin over the limit latches it and fails Stop.
func TestRetainedFileContentIsNotChargedToTheAuditLimit(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		call    func(sandboxv1.SandboxClient) error
		latches bool
	}{
		{"retained file content", func(client sandboxv1.SandboxClient) error {
			_, err := writeFile(client, "/app/f", 64<<10, make([]byte, 64<<10))
			return err
		}, false},
		{"stdin", func(client sandboxv1.SandboxClient) error {
			_, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: catPayload, Stdin: make([]byte, 8<<10)})
			return err
		}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manager, err := New(Options{OutputDir: t.TempDir(), ClientPath: fakeClient(t), RetainContent: true})
			if err != nil {
				t.Fatal(err)
			}
			manager.auditLimit = 4 << 10
			endpoint, err := manager.Start(context.Background(), newMemorySandbox(fstest.MapFS{}))
			if err != nil {
				t.Fatal(err)
			}
			client, closeClient := dial(t, endpoint)
			defer closeClient()
			if err := testCase.call(client); err != nil {
				t.Fatal(err)
			}
			if err := manager.Stop(context.Background()); (err != nil) != testCase.latches {
				t.Fatalf("Stop() = %v, want latched %v", err, testCase.latches)
			}
		})
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
			var out bytes.Buffer
			got, err := readLines(strings.NewReader(content), &out, first, count, clamp)
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
			if out.String() != want || got.GetTotalLines() != total {
				t.Fatalf("content %q window %v:\ngot  %q (%d lines)\nwant %q (%d lines)", content[:min(len(content), 20)], window, out.String(), got.GetTotalLines(), want, total)
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
