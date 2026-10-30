package grpc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os/exec"
	"slices"
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
	probe, header, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/new.txt", MaxBytes: 3})
	if err != nil || string(probe) != "one" || header.GetSize() != 8 || !header.GetTruncated() {
		t.Fatalf("read head = %q %v, %v", probe, header, err)
	}
	unbounded, header, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/new.txt", MaxBytes: math.MaxInt64})
	if err != nil || string(unbounded) != "one\ntwo\n" || header.GetTruncated() {
		t.Fatalf("read with the largest bound = %q %v, %v", unbounded, header, err)
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
	manager, client, endpoint := startBridge(t, executor, Options{})
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
		"missing file by lines": {func() error {
			_, _, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/none", FirstLine: 1, MaxLines: 1})
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
	stop(t, manager)
	// Failed and rejected calls also record an E2B operation, and a line read
	// that returned no summary records no total_lines.
	for _, record := range readToolCalls(t, endpoint.LogPaths[0]) {
		if !slices.Contains(e2bOperations, record["operation_class"].(string)) {
			t.Fatalf("record outside the E2B operations: %#v", record)
		}
		if _, present := record["total_lines"]; present {
			t.Fatalf("a failed call records total_lines: %#v", record)
		}
	}
}

// The container root is a directory, as the backends report it: Stat finds a
// directory, and both reads refuse it as not a regular file.
func TestContainerRootIsADirectory(t *testing.T) {
	_, client, _ := startBridge(t, newFakeExecutor(nil), Options{})
	info, err := client.Stat(context.Background(), &sandboxv1.StatRequest{Path: "/"})
	if err != nil || !info.GetExists() || info.GetType() != sandboxv1.FileType_FILE_TYPE_DIRECTORY {
		t.Fatalf("Stat(/) = %v, %v", info, err)
	}
	if _, _, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ReadFile(/) = %v, want FAILED_PRECONDITION", err)
	}
	if _, _, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/", FirstLine: 1, MaxLines: 1}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ReadLines(/) = %v, want FAILED_PRECONDITION", err)
	}
}

// e2bOperations is the operation_class set of gRPC records.
var e2bOperations = []string{"exec", "stat", "read", "write"}

// TestToolCallRecordsFollowTheSSHRules pins the shared record rules: clean
// UTF-8 inline, a binary-omitted note for binary, no base64 field, WriteFile
// content as stdin, and reads with a digest and a byte count but no content.
// Each started_at is the real start: after the send, before the sandbox work.
// Both read procedures record as read with their window; other calls have none.
func TestToolCallRecordsFollowTheSSHRules(t *testing.T) {
	// The window values differ from each other, so a swapped field shows.
	const firstLine, maxLines, maxLineBytes, maxBytes = 1, 2, 3, 4
	executor := newFakeExecutor(fstest.MapFS{
		"app/r":     &fstest.MapFile{Data: []byte("read-me\n"), Mode: 0o644},
		"app/empty": &fstest.MapFile{Mode: 0o644},
	})
	var written []time.Time
	executor.writeHook = func(context.Context) error {
		written = append(written, time.Now())
		return nil
	}
	manager, client, endpoint := startBridge(t, executor, Options{OmitRawLog: true})
	ctx := context.Background()
	binary := []byte{0, 1, 2, 0xff}
	sent := time.Now()
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
	if _, _, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/r", MaxBytes: maxBytes}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/empty", FirstLine: firstLine, MaxLines: maxLines, MaxLineBytes: maxLineBytes}); err != nil {
		t.Fatal(err)
	}
	stop(t, manager)

	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 9 {
		t.Fatalf("records = %#v", records)
	}
	textDigest := sha256.Sum256([]byte("text-file"))
	readDigest := sha256.Sum256([]byte("read-me\n"))
	for index, want := range []map[string]any{
		{"operation_class": "exec", "stdin": "text-stdin", "stdin_encoding": "utf-8"},
		{"operation_class": "exec", "stdin": fmt.Sprintf("[binary input omitted; %d bytes not retained]", len(binary)), "stdin_encoding": "binary-omitted", "stdin_bytes": float64(len(binary))},
		{"operation_class": "write", "path": "/app/w", "stdin": "text-file", "stdin_encoding": "utf-8", "sha256": hex.EncodeToString(textDigest[:]), "exit_code": float64(-1)},
		{"operation_class": "write", "path": "/app/b", "stdin_encoding": "binary-omitted", "stdin_bytes": float64(len(binary))},
		{"operation_class": "read", "path": "/app/r", "stdin": "", "stdout_bytes": float64(len("read-me\n")), "sha256": hex.EncodeToString(readDigest[:])},
		{"operation_class": "read", "path": "/app/r", "stdin": "", "stdout_bytes": float64(len("read-me\n")), "sha256": hex.EncodeToString(readDigest[:]),
			"first_line": float64(1), "max_lines": float64(1), "total_lines": float64(1)},
		{"operation_class": "stat", "path": "/app/r", "status": "completed", "exit_code": float64(-1)},
		{"operation_class": "read", "path": "/app/r", "stdout_bytes": float64(maxBytes), "truncated": true, "max_bytes": float64(maxBytes)},
		{"operation_class": "read", "path": "/app/empty", "stdout_bytes": float64(0),
			"first_line": float64(firstLine), "max_lines": float64(maxLines), "max_line_bytes": float64(maxLineBytes), "total_lines": float64(0)},
	} {
		for field, value := range want {
			if records[index][field] != value {
				t.Fatalf("record %d [%q] = %v, want %v", index, field, records[index][field], value)
			}
		}
		for _, field := range []string{"max_bytes", "first_line", "max_lines", "max_line_bytes", "total_lines"} {
			if _, wanted := want[field]; !wanted {
				if _, present := records[index][field]; present {
					t.Fatalf("record %d carries window field %q: %#v", index, field, records[index])
				}
			}
		}
		for _, forbidden := range []string{"stdin_raw", "content_raw", "request_type", "want_reply"} {
			if _, present := records[index][forbidden]; present {
				t.Fatalf("record %d carries %q", index, forbidden)
			}
		}
	}
	sandboxEntries := slices.Concat(executor.entered, written)
	for index, record := range records {
		started := recordStartedAt(t, record)
		if started.Before(sent) {
			t.Fatalf("record %d started_at %v is before the send at %v", index, started, sent)
		}
		// Records 0-1 are the Exec calls and 2-3 the writes, in sandbox order.
		if index < len(sandboxEntries) && started.After(sandboxEntries[index]) {
			t.Fatalf("record %d started_at %v is after the sandbox work at %v", index, started, sandboxEntries[index])
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

// A client that goes away mid-stream ends the call as a cancellation: the
// record says canceled, and revocation confirms.
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

// A target error during cancellation does not fail revocation: the command
// may still run in the sandbox, and only the handler join and the audit
// confirm the release. The record keeps the error.
func TestTargetErrorDuringCancellationDoesNotFailRevocation(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.writeHook = func(ctx context.Context) error {
		<-ctx.Done()
		return errors.New("rename not confirmed")
	}
	manager, client, endpoint := startBridge(t, executor, Options{})
	go func() { _, _ = writeFile(client, "/new", 1, []byte("x")) }()
	waitForFileOps(executor, 1)
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v, want confirmed revocation", err)
	}
	if records := readToolCalls(t, endpoint.LogPaths[0]); len(records) != 1 || records[0]["status"] != "failed" {
		t.Fatalf("records = %v", records)
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

// The window arithmetic must not overflow at the edges of int64.
func TestReadLinesWindowNeverOverflows(t *testing.T) {
	for _, test := range []struct {
		first, count int64
		window       string
		more         bool
	}{
		{first: 2, count: math.MaxInt64, window: "b\n"},
		{first: math.MaxInt64, count: 2, window: ""},
	} {
		var out bytes.Buffer
		summary, err := readLines(strings.NewReader("a\nb\n"), &out, test.first, test.count, 0)
		if err != nil || out.String() != test.window || summary.GetMore() != test.more || summary.GetTotalLines() != 2 {
			t.Fatalf("readLines(first=%d, count=%d) = %q %v, %v", test.first, test.count, out.String(), summary, err)
		}
	}
}

// ReadFile had an offset field with no caller. Its number and name stay
// reserved, so an old client cannot reuse them with another meaning.
func TestReadFileRequestReservesTheRemovedOffset(t *testing.T) {
	const offsetField = 2 // the removed field's number in sandbox.proto
	descriptor := (&sandboxv1.ReadFileRequest{}).ProtoReflect().Descriptor()
	if !descriptor.ReservedRanges().Has(offsetField) || !descriptor.ReservedNames().Has("offset") {
		t.Fatal("ReadFileRequest does not reserve the removed offset field")
	}
}
