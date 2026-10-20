package grpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestFileProceduresMoveBytesThroughTheSandbox(t *testing.T) {
	executor := newFakeExecutor(fstest.MapFS{
		"app/src":     &fstest.MapFile{Mode: fs.ModeDir | 0o755},
		"app/old.bin": &fstest.MapFile{Data: []byte{0, 1, 2, 0xff}, Mode: 0o600},
	})
	_, client, _ := startBridge(t, executor, Options{})
	ctx := context.Background()

	written, err := writeFile(client, "/app/new.txt", 8, []byte("one\ntwo\n"))
	if err != nil || written.GetBytesWritten() != 8 || !written.GetCreated() {
		t.Fatalf("create = %v, %v", written, err)
	}
	replaced, err := writeFile(client, "/app/old.bin", 4, []byte("text"))
	if err != nil || replaced.GetCreated() {
		t.Fatalf("replace = %v, %v", replaced, err)
	}
	if empty, err := writeFile(client, "/app/empty", 0, nil); err != nil || !empty.GetCreated() || len(executor.files["app/empty"].Data) != 0 {
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

// File content has no size bound: a body over the retained bound and over
// grpc-go's default message size crosses in chunks both ways. Its content is
// left out of the audit, but its size and digest are kept and Stop confirms.
func TestFileContentLargerThanTheRetainedBoundStreams(t *testing.T) {
	executor := newFakeExecutor(nil)
	manager, client, endpoint := startBridge(t, executor, Options{})
	line := []byte(strings.Repeat("x", 99) + "\n")
	body := bytes.Repeat(line, bridgekit.MaxRecordedInputBytes/len(line)+1)

	if _, err := writeFile(client, "/app/big", int64(len(body)), body); err != nil {
		t.Fatal(err)
	}
	got, header, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/big"})
	if err != nil || !bytes.Equal(got, body) || header.GetSize() != int64(len(body)) {
		t.Fatalf("read back %d bytes (header %v), want %d: %v", len(got), header, len(body), err)
	}
	window, summary, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/big", FirstLine: 1, MaxLines: int64(len(body))})
	if err != nil || !bytes.Equal(window, body) || summary.GetTotalLines() != int64(len(body)/len(line)) || summary.GetMore() {
		t.Fatalf("window of %d bytes (summary %v), want the whole file: %v", len(window), summary, err)
	}
	stop(t, manager)
	digest := sha256.Sum256(body)
	record := readToolCalls(t, endpoint.LogPaths[0])[0]
	if record["stdin_encoding"] != "size-omitted" || record["stdin_bytes"] != float64(len(body)) || record["sha256"] != hex.EncodeToString(digest[:]) {
		t.Fatalf("record = %v", record)
	}
	if stdin := record["stdin"].(string); !strings.Contains(stdin, strconv.Itoa(len(body))) || len(stdin) > len(line) {
		t.Fatalf("stdin = %q, want a short note with the byte count", stdin)
	}
}

func TestFileProceduresMapFailuresToStatuses(t *testing.T) {
	executor := newFakeExecutor(fstest.MapFS{
		"app/dir":  &fstest.MapFile{Mode: fs.ModeDir | 0o755},
		"app/kept": &fstest.MapFile{Data: []byte("old"), Mode: 0o644},
	})
	_, client, _ := startBridge(t, executor, Options{})
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
		"chunk before header": {func() error {
			stream, err := client.WriteFile(ctx)
			if err != nil {
				return err
			}
			_ = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Chunk{Chunk: []byte("new")}})
			_, err = stream.CloseAndRecv()
			return err
		}, codes.InvalidArgument},
		"second header": {func() error {
			stream, err := client.WriteFile(ctx)
			if err != nil {
				return err
			}
			header := &sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Header{Header: &sandboxv1.WriteFileHeader{Path: "/app/kept", Size: 1}}}
			_ = stream.Send(header)
			_ = stream.Send(header)
			_, err = stream.CloseAndRecv()
			return err
		}, codes.InvalidArgument},
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
	if kept := string(executor.files["app/kept"].Data); kept != "old" {
		t.Fatalf("a malformed write stream changed the file to %q", kept)
	}
}

// TestToolCallRecordsFollowTheSSHRules pins the shared record rules: clean
// UTF-8 inline, a binary-omitted note for binary, no base64 field, WriteFile
// content as stdin, and reads with a digest and a byte count but no content.
func TestToolCallRecordsFollowTheSSHRules(t *testing.T) {
	executor := newFakeExecutor(fstest.MapFS{"app/r": &fstest.MapFile{Data: []byte("read-me\n"), Mode: 0o644}})
	manager, client, endpoint := startBridge(t, executor, Options{OmitRawLog: true})
	ctx := context.Background()
	binary := []byte{0, 1, 2, 0xff}
	_, _ = client.Exec(ctx, &sandboxv1.ExecRequest{Script: "cat", Stdin: []byte("text-stdin")})
	_, _ = client.Exec(ctx, &sandboxv1.ExecRequest{Script: "cat", Stdin: binary})
	if _, err := writeFile(client, "/app/w", int64(len("text-file")), []byte("text-file")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeFile(client, "/app/b", int64(len(binary)), binary); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/r"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/r", FirstLine: 1, MaxLines: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stat(ctx, &sandboxv1.StatRequest{Path: "/app/r"}); err != nil {
		t.Fatal(err)
	}
	stop(t, manager)

	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 7 {
		t.Fatalf("records = %#v", records)
	}
	textDigest := sha256.Sum256([]byte("text-file"))
	readDigest := sha256.Sum256([]byte("read-me\n"))
	for index, want := range []map[string]any{
		{"operation_class": "agent", "stdin": "text-stdin", "stdin_encoding": "utf-8"},
		{"operation_class": "agent", "stdin": fmt.Sprintf("[binary input omitted; %d bytes not retained]", len(binary)), "stdin_encoding": "binary-omitted", "stdin_bytes": float64(len(binary))},
		{"operation_class": "WriteFile", "path": "/app/w", "stdin": "text-file", "stdin_encoding": "utf-8", "sha256": hex.EncodeToString(textDigest[:]), "exit_code": float64(-1)},
		{"operation_class": "WriteFile", "path": "/app/b", "stdin_encoding": "binary-omitted", "stdin_bytes": float64(len(binary))},
		{"operation_class": "ReadFile", "path": "/app/r", "stdin": "", "stdout_bytes": float64(len("read-me\n")), "sha256": hex.EncodeToString(readDigest[:])},
		{"operation_class": "ReadLines", "path": "/app/r", "stdin": "", "stdout_bytes": float64(len("read-me\n")), "sha256": hex.EncodeToString(readDigest[:])},
		{"operation_class": "Stat", "path": "/app/r", "status": "completed", "exit_code": float64(-1)},
	} {
		for field, value := range want {
			if records[index][field] != value {
				t.Fatalf("record %d [%q] = %v, want %v", index, field, records[index][field], value)
			}
		}
		for _, forbidden := range []string{"stdin_raw", "content_raw", "request_type", "want_reply"} {
			if _, present := records[index][forbidden]; present {
				t.Fatalf("record %d carries %q", index, forbidden)
			}
		}
	}
	for _, record := range records {
		for _, value := range record {
			if text, ok := value.(string); ok && strings.Contains(text, "read-me") {
				t.Fatalf("a record holds read content: %#v", record)
			}
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

// streamedBytes is far above gRPC's flow-control window, so a read handler is
// still sending when its client goes away.
const streamedBytes = 32 << 20

// waitForFileOps polls until the target has seen count file calls.
func waitForFileOps(executor *fakeExecutor, count int) {
	for {
		if _, _, ops := executor.snapshot(); ops >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// A client that goes away mid-stream ends the call as a pure cancellation: the
// failed stream operation must not make revocation look unconfirmed.
func TestClientCancelMidStreamStillConfirmsRevocation(t *testing.T) {
	for name, start := range map[string]func(context.Context, sandboxv1.SandboxClient, *fakeExecutor) error{
		"ReadFile": func(ctx context.Context, client sandboxv1.SandboxClient, _ *fakeExecutor) error {
			stream, err := client.ReadFile(ctx, &sandboxv1.ReadFileRequest{Path: "/big"})
			if err == nil {
				_, err = stream.Recv()
			}
			return err
		},
		"ReadLines": func(ctx context.Context, client sandboxv1.SandboxClient, _ *fakeExecutor) error {
			stream, err := client.ReadLines(ctx, &sandboxv1.ReadLinesRequest{Path: "/big", FirstLine: 1, MaxLines: streamedBytes})
			if err == nil {
				_, err = stream.Recv()
			}
			return err
		},
		"WriteFile": func(ctx context.Context, client sandboxv1.SandboxClient, executor *fakeExecutor) error {
			stream, err := client.WriteFile(ctx)
			if err == nil {
				// The header promises more than is sent, so the handler waits
				// for the rest.
				err = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Header{Header: &sandboxv1.WriteFileHeader{Path: "/new", Size: 2}}})
			}
			if err == nil {
				err = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Chunk{Chunk: []byte("x")}})
			}
			waitForFileOps(executor, 1)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			executor := newFakeExecutor(fstest.MapFS{"big": {Data: bytes.Repeat([]byte("line\n"), streamedBytes/len("line\n")), Mode: 0o644}})
			manager, client, endpoint := startBridge(t, executor, Options{})
			ctx, cancel := context.WithCancel(context.Background())
			if err := start(ctx, client, executor); err != nil {
				t.Fatal(err)
			}
			cancel()
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatalf("Stop() = %v, want revocation confirmed", err)
			}
			if records := readToolCalls(t, endpoint.LogPaths[0]); len(records) != 1 || records[0]["status"] != "canceled" {
				t.Fatalf("records = %v", records)
			}
		})
	}
}

// A target error after cancellation is not a pure cancellation: it may hide
// an unconfirmed write, so revocation fails closed.
func TestTargetErrorAfterCancelFailsRevocation(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.writeHook = func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("rename not confirmed")
	}
	manager, client, _ := startBridge(t, executor, Options{})
	go func() { _, _ = writeFile(client, "/new", 1, []byte("x")) }()
	waitForFileOps(executor, 1)
	if err := manager.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "rename not confirmed") {
		t.Fatalf("Stop() = %v, want the target error", err)
	}
}

// A write the target confirmed is completed even when its client is gone
// before the response: the record says what happened in the sandbox.
func TestConfirmedWriteIsCompletedWhenTheResponseIsLost(t *testing.T) {
	executor := newFakeExecutor(nil)
	ctx, cancel := context.WithCancel(context.Background())
	executor.writeHook = func(callCtx context.Context) error {
		cancel()
		<-callCtx.Done()
		return nil
	}
	manager, client, endpoint := startBridge(t, executor, Options{})
	stream, err := client.WriteFile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Header{Header: &sandboxv1.WriteFileHeader{Path: "/new", Size: 1}}})
	_ = stream.Send(&sandboxv1.WriteFileRequest{Part: &sandboxv1.WriteFileRequest_Chunk{Chunk: []byte("x")}})
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.Canceled {
		t.Fatalf("CloseAndRecv() = %v, want the client's own cancellation", err)
	}
	stop(t, manager)
	if records := readToolCalls(t, endpoint.LogPaths[0]); len(records) != 1 || records[0]["status"] != "completed" {
		t.Fatalf("records = %v", records)
	}
}
