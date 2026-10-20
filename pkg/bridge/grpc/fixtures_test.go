package grpc

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const testWorkdir = "/app"

// fakeExecutor is a borrowed target over an in-memory filesystem. Paths are
// absolute on the wire and unrooted in fstest.MapFS.
type fakeExecutor struct {
	workdir        string
	exitCode       int
	stdout, stderr []byte
	// block holds ExecStream until its context ends; lag then delays the
	// return, as a sandbox does while it confirms termination.
	block bool
	lag   time.Duration
	// writeHook runs after WriteFile stores the file, as the Docker target
	// confirms its rename; its error is the target's.
	writeHook func(context.Context) error

	mu       sync.Mutex
	commands []core.Command
	stdins   [][]byte
	fileOps  int
	files    fstest.MapFS
}

func newFakeExecutor(files fstest.MapFS) *fakeExecutor {
	if files == nil {
		files = fstest.MapFS{}
	}
	return &fakeExecutor{workdir: testWorkdir, files: files}
}

func (executor *fakeExecutor) ContainerID() string   { return "sandbox-container-id" }
func (executor *fakeExecutor) ContainerName() string { return "sandbox-container-name" }
func (executor *fakeExecutor) RunID() string         { return "test-run" }
func (executor *fakeExecutor) TaskID() string        { return "test-task" }
func (executor *fakeExecutor) Workdir() string       { return executor.workdir }

func (executor *fakeExecutor) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	input, err := io.ReadAll(stdin)
	if err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	executor.mu.Lock()
	executor.commands = append(executor.commands, command)
	executor.stdins = append(executor.stdins, input)
	executor.mu.Unlock()
	if executor.block {
		<-ctx.Done()
		time.Sleep(executor.lag)
		return core.CommandResult{ExitCode: -1}, ctx.Err()
	}
	if _, err := stdout.Write(executor.stdout); err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	if _, err := stderr.Write(executor.stderr); err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	return core.CommandResult{ExitCode: executor.exitCode}, nil
}

func (executor *fakeExecutor) StatFile(_ context.Context, path string) (fs.FileInfo, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.fileOps++
	return fs.Stat(executor.files, strings.TrimPrefix(path, "/"))
}

func (executor *fakeExecutor) OpenFile(_ context.Context, path string) (io.ReadCloser, fs.FileInfo, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.fileOps++
	file, err := executor.files.Open(strings.TrimPrefix(path, "/"))
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	return file, info, err
}

// WriteFile holds the whole body before it touches the file, and reads to
// io.EOF as the Docker backend does, so a malformed stream changes nothing.
func (executor *fakeExecutor) WriteFile(ctx context.Context, path string, content io.Reader, size int64) (bool, error) {
	executor.mu.Lock()
	executor.fileOps++
	executor.mu.Unlock()
	data, err := io.ReadAll(content)
	if err != nil {
		return false, err
	}
	if int64(len(data)) != size {
		return false, io.ErrUnexpectedEOF
	}
	executor.mu.Lock()
	name := strings.TrimPrefix(path, "/")
	mode := fs.FileMode(0o644)
	info, err := fs.Stat(executor.files, name)
	if err == nil && info.IsDir() {
		executor.mu.Unlock()
		return false, syscall.EISDIR
	}
	if err == nil {
		mode = info.Mode()
	}
	executor.files[name] = &fstest.MapFile{Data: data, Mode: mode}
	executor.mu.Unlock()
	if executor.writeHook != nil {
		return err != nil, executor.writeHook(ctx)
	}
	return err != nil, nil
}

func (executor *fakeExecutor) snapshot() ([]core.Command, [][]byte, int) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]core.Command(nil), executor.commands...), append([][]byte(nil), executor.stdins...), executor.fileOps
}

func loopbackListen(context.Context) (core.BridgeListen, error) {
	return core.BridgeListen{BindHost: "127.0.0.1"}, nil
}

func newTestManager(t *testing.T, options Options) *Manager {
	t.Helper()
	if options.OutputDir == "" {
		options.OutputDir = t.TempDir()
	}
	if options.SandboxID == "" {
		options.SandboxID = "test-sandbox"
	}
	if options.ResolveListen == nil {
		options.ResolveListen = loopbackListen
	}
	manager, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	return manager
}

// dial connects as the shipped client does: plaintext, no credentials. The
// proxy is skipped so a host proxy setting cannot divert a loopback dial.
func dial(t *testing.T, endpoint core.ToolEndpoint) sandboxv1.SandboxClient {
	t.Helper()
	connection, err := grpc.NewClient(endpoint.Address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy(),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMessageBytes)))
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return sandboxv1.NewSandboxClient(connection)
}

func startBridge(t *testing.T, executor *fakeExecutor, options Options) (*Manager, sandboxv1.SandboxClient, core.ToolEndpoint) {
	t.Helper()
	manager := newTestManager(t, options)
	endpoint, err := manager.StartTarget(context.Background(), executor)
	if err != nil {
		t.Fatalf("StartTarget() error = %v", err)
	}
	return manager, dial(t, endpoint), endpoint
}

func stop(t *testing.T, manager *Manager) {
	t.Helper()
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func readToolCalls(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 2*maxMessageBytes)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("tool-calls line is not JSON: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
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
