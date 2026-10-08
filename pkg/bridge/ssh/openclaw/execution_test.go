package openclaw

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	bridgessh "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/internal/testfixture"
	"github.com/hyscale-lab/aries/pkg/core"
	gossh "golang.org/x/crypto/ssh"
)

type recordingExecutor struct {
	mu       sync.Mutex
	commands []core.Command
}

func (*recordingExecutor) ContainerID() string   { return "container" }
func (*recordingExecutor) ContainerName() string { return "container" }
func (*recordingExecutor) RunID() string         { return "run" }
func (*recordingExecutor) TaskID() string        { return "task" }
func (*recordingExecutor) Workdir() string       { return "/workspace" }
func (s *recordingExecutor) ExecStream(_ context.Context, c core.Command, _ io.Reader, _, _ io.Writer) (core.CommandResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, c)
	return core.CommandResult{}, nil
}
func (s *recordingExecutor) snapshot() []core.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Command(nil), s.commands...)
}

func executeAndAudit(t *testing.T, wire string, input []byte) ([]core.Command, []map[string]any, []byte, []byte) {
	t.Helper()
	sandbox := &recordingExecutor{}
	server := testfixture.New(t, bridgessh.Options{Dialect: Dialect{}, OutputDir: t.TempDir()})
	endpoint, err := server.StartTarget(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := gossh.Dial("tcp", endpoint.Address, &gossh.ClientConfig{User: endpoint.Username, HostKeyCallback: gossh.InsecureIgnoreHostKey()})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	session.Stdin = bytes.NewReader(input)
	if err := session.Run(wire); err != nil {
		t.Fatal(err)
	}
	if err := server.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	structured, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(endpoint.LogPaths[1])
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.snapshot(), testfixture.ReadToolCalls(t, endpoint.LogPaths[0]), structured, raw
}

func TestVirtualizedExecutionKeepsWireEvidenceAndRecordsExecutedState(t *testing.T) {
	wire := encodeCanonicalTokens(generatedArgv("cd " + virtualWorkspace + " && cat " + virtualWorkspace + "/input >" + virtualWorkspace + "/output"))
	commands, records, _, raw := executeAndAudit(t, wire, nil)
	if len(commands) != 1 || commands[0].Args[1] != "cd /workspace && cat /workspace/input >/workspace/output" || commands[0].Env["HOME"] != generatedHome {
		t.Fatalf("executed commands = %#v", commands)
	}
	if len(records) != 1 || records[0]["workspace_home"] != generatedHome || records[0]["command"] != commands[0].Args[1] {
		t.Fatalf("structured record = %#v", records)
	}
	prepared, refusal := (Dialect{}).Prepare(wire, "/workspace")
	if refusal != nil {
		t.Fatal(refusal)
	}
	hash := func(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }
	if records[0]["command_hash"] != hash(prepared.HashInput) || records[0]["command_hash"] == hash(wire) {
		t.Fatalf("hash = %#v", records[0])
	}
	if !reflect.DeepEqual(records[0]["env_names"], []any{"HOME", "LANG", "OPENCLAW_SHELL", "PATH"}) {
		t.Fatalf("env names = %#v", records[0])
	}
	rawRecords := testfixture.DecodeRawAuditRecords(t, raw)
	if len(rawRecords) != 1 || string(testfixture.UnescapeRawValue(t, rawRecords[0]["wire_command"])) != wire || !bytes.Equal(testfixture.UnescapeRawValue(t, rawRecords[0]["payload"]), gossh.Marshal(struct{ Command string }{wire})) {
		t.Fatalf("raw = %#v", rawRecords)
	}
}

func TestSuppressedTransportCleanupNeverExecutesSandbox(t *testing.T) {
	for _, argv := range [][]string{
		{remoteShell, "-c", directoryClearScript, directoryClearLabel, virtualSkillsWorkspace, virtualRuntimeRoot},
		{remoteShell, "-c", runtimeRemoveScript, runtimeRemoveLabel, virtualRuntimeRoot},
	} {
		commands, records, _, _ := executeAndAudit(t, encodeCanonicalTokens(argv), nil)
		if len(commands) != 0 || len(records) != 1 || records[0]["status"] != "completed" {
			t.Fatalf("cleanup commands=%#v records=%#v", commands, records)
		}
	}
}

func TestSuppressedSkillsUploadDrainsInputAndPreservesStructuredClassification(t *testing.T) {
	argv := []string{remoteShell, "-c", directoryUploadScript, directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot}
	upload := []byte("tar\x00stream")
	commands, records, structured, raw := executeAndAudit(t, encodeCanonicalTokens(argv), upload)
	if len(commands) != 0 || len(records) != 1 {
		t.Fatalf("commands=%#v records=%#v", commands, records)
	}
	record := records[0]
	if record["operation_class"] != "workspace_upload" || record["stdin"] != "[binary input omitted; 10 bytes retained in ssh_raw.log]" || record["stdin_encoding"] != "binary-omitted" || record["stdin_bytes"] != float64(len(upload)) {
		t.Fatalf("upload record = %#v", record)
	}
	if bytes.Contains(structured, []byte(`\u0000`)) || bytes.Contains(structured, []byte{0}) {
		t.Fatalf("structured NUL = %q", structured)
	}
	if bytes.Contains(raw, []byte{0}) || !bytes.Contains(raw, []byte(`stdin=tar\x00stream`)) {
		t.Fatalf("raw input = %q", raw)
	}
	for _, omitted := range []string{"command", "workspace_home", "env_names", "error"} {
		if _, found := record[omitted]; found {
			t.Fatalf("unexpected %s = %#v", omitted, record)
		}
	}
	gotArgv := record["argv"].([]any)
	var values []string
	for _, v := range gotArgv {
		values = append(values, v.(string))
	}
	if strings.Join(values, "\x00") != strings.Join(argv, "\x00") {
		t.Fatalf("argv = %#v", gotArgv)
	}
}
