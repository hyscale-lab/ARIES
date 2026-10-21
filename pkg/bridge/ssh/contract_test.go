package ssh_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	bridgessh "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/hermes"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/internal/testfixture"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw"
	sshclient "github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/client"
	"github.com/hyscale-lab/aries/pkg/core"
	gossh "golang.org/x/crypto/ssh"
)

type nativeCase struct {
	name                 string
	dialect              bridgessh.Dialect
	command, path, class string
}

var nativeCases = []nativeCase{
	{"hermes", hermes.Dialect{}, "bash -c true", "/bin/bash", "agent"},
	{"openclaw", openclaw.Dialect{}, "'/bin/sh' '-c' 'true'", "/bin/sh", "exec"},
}

type contractExecutor struct {
	mu    sync.Mutex
	calls []core.Command
	input []byte
}

func (*contractExecutor) ContainerID() string   { return "container" }
func (*contractExecutor) ContainerName() string { return "container" }
func (*contractExecutor) RunID() string         { return "run" }
func (*contractExecutor) TaskID() string        { return "task" }
func (*contractExecutor) Workdir() string       { return "/workspace" }
func (e *contractExecutor) ExecStream(_ context.Context, c core.Command, in io.Reader, out, errout io.Writer) (core.CommandResult, error) {
	data, err := io.ReadAll(in)
	if err != nil {
		return core.CommandResult{}, err
	}
	e.mu.Lock()
	e.calls = append(e.calls, c)
	e.input = bytes.Clone(data)
	e.mu.Unlock()
	_, _ = out.Write(data)
	_, _ = errout.Write([]byte{255, 0, 'e'})
	return core.CommandResult{ExitCode: 7}, nil
}
func (e *contractExecutor) snapshot() ([]core.Command, []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]core.Command(nil), e.calls...), bytes.Clone(e.input)
}

type nativeFixture struct {
	ctx      context.Context
	server   *testfixture.Server
	endpoint core.ToolEndpoint
	target   *contractExecutor
	config   sshclient.Config
}

func startNative(t *testing.T, tc nativeCase) *nativeFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	server := testfixture.New(t, bridgessh.Options{Dialect: tc.dialect, OutputDir: t.TempDir()})
	target := &contractExecutor{}
	endpoint, err := server.StartTarget(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	return &nativeFixture{ctx, server, endpoint, target, sshclient.Config{Address: endpoint.Address, User: endpoint.Username}}
}
func (f *nativeFixture) dial(t *testing.T) *gossh.Client {
	t.Helper()
	client, err := gossh.Dial("tcp", f.config.Address, &gossh.ClientConfig{User: f.config.User, HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stop := context.AfterFunc(f.ctx, func() { _ = client.Close() })
	t.Cleanup(func() { stop(); _ = client.Close() })
	return client
}
func (f *nativeFixture) records(t *testing.T) []map[string]any {
	t.Helper()
	if err := f.server.Stop(f.ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	return records
}

func TestProductionDialectsShareStreamingAndEvidenceContract(t *testing.T) {
	for _, tc := range nativeCases {
		t.Run(tc.name, func(t *testing.T) {
			f := startNative(t, tc)
			input := []byte{0, 255, 'x', '\n'}
			var out, errout bytes.Buffer
			code, err := sshclient.Run(f.ctx, f.config, tc.command, io.NopCloser(bytes.NewReader(input)), &out, &errout)
			if code != 7 || err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			calls, gotInput := f.target.snapshot()
			if len(calls) != 1 || calls[0].Path != tc.path || !reflect.DeepEqual(calls[0].Args, []string{"-c", "true"}) || calls[0].Dir != "/workspace" {
				t.Fatalf("commands=%#v", calls)
			}
			if !bytes.Equal(input, gotInput) || !bytes.Equal(input, out.Bytes()) || !bytes.Equal(errout.Bytes(), []byte{255, 0, 'e'}) {
				t.Fatal("binary streams changed")
			}
			records := f.records(t)
			if len(records) != 1 {
				t.Fatalf("records=%#v", records)
			}
			r := records[0]
			if r["status"] != "completed" || r["operation_class"] != tc.class || r["exit_code"] != float64(7) || r["stdin_bytes"] != float64(len(input)) || r["command_hash"] != fmt.Sprintf("%x", sha256.Sum256([]byte(tc.command))) {
				t.Fatalf("record=%#v", r)
			}
		})
	}
}

func TestProductionDialectsPreserveUnsupportedChannelPolicies(t *testing.T) {
	for _, tc := range nativeCases {
		for _, requestType := range []string{"env", "pty-req"} {
			t.Run(tc.name+"/"+requestType, func(t *testing.T) {
				f := startNative(t, tc)
				client := f.dial(t)
				channel, requests, err := client.OpenChannel("session", nil)
				if err != nil {
					t.Fatal(err)
				}
				defer channel.Close()
				go gossh.DiscardRequests(requests)
				accepted, err := channel.SendRequest(requestType, true, gossh.Marshal(struct{ Name, Value string }{"LANG", "C"}))
				if err != nil || accepted {
					t.Fatalf("unsupported accepted=%v err=%v", accepted, err)
				}
				if tc.name == "openclaw" {
					if _, err := io.ReadAll(channel); err != nil {
						t.Fatal(err)
					}
					accepted, err := channel.SendRequest("exec", true, gossh.Marshal(struct{ Command string }{tc.command}))
					if err == nil && accepted {
						t.Fatal("terminal refusal admitted later exec")
					}
				} else {
					accepted, err := channel.SendRequest("exec", true, gossh.Marshal(struct{ Command string }{tc.command}))
					if err != nil || !accepted {
						t.Fatalf("following exec accepted=%v err=%v", accepted, err)
					}
					_ = channel.CloseWrite()
					if _, err := io.ReadAll(channel); err != nil {
						t.Fatal(err)
					}
				}
				records := f.records(t)
				calls, _ := f.target.snapshot()
				expected := 1
				status := "rejected"
				if tc.name == "hermes" {
					expected = 2
					status = "unsupported"
				}
				if len(records) != expected || records[0]["request_type"] != requestType || records[0]["status"] != status || len(calls) != expected-1 {
					t.Fatalf("records=%#v calls=%#v", records, calls)
				}
				if tc.name == "hermes" && records[1]["status"] != "completed" {
					t.Fatalf("following exec=%#v", records[1])
				}
			})
		}
	}
}

func TestProductionDialectsRejectExecWithoutReplyAndAudit(t *testing.T) {
	for _, tc := range nativeCases {
		t.Run(tc.name, func(t *testing.T) {
			f := startNative(t, tc)
			client := f.dial(t)
			channel, requests, err := client.OpenChannel("session", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer channel.Close()
			go gossh.DiscardRequests(requests)
			if _, err := channel.SendRequest("exec", false, gossh.Marshal(struct{ Command string }{tc.command})); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadAll(channel); err != nil {
				t.Fatal(err)
			}
			records := f.records(t)
			calls, _ := f.target.snapshot()
			if len(calls) != 0 || len(records) != 1 || records[0]["status"] != "rejected" || records[0]["request_type"] != "exec" || records[0]["want_reply"] != false {
				t.Fatalf("calls=%#v records=%#v", calls, records)
			}
		})
	}
}

func TestThinClientUnknownCommandReachesProductionRefusalAudit(t *testing.T) {
	for _, tc := range nativeCases {
		t.Run(tc.name, func(t *testing.T) {
			f := startNative(t, tc)
			const unknown = "unrecognized 'command'\nλ; $(false)"
			code, err := sshclient.Run(f.ctx, f.config, unknown, io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
			if code != 255 || err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("code=%d err=%v", code, err)
			}
			records := f.records(t)
			calls, _ := f.target.snapshot()
			if len(calls) != 0 || len(records) != 1 || records[0]["status"] != "rejected" || records[0]["command_hash"] != fmt.Sprintf("%x", sha256.Sum256([]byte(unknown))) {
				t.Fatalf("calls=%#v records=%#v", calls, records)
			}
			raw, err := os.ReadFile(f.endpoint.LogPaths[1])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(raw, []byte("unrecognized")) {
				t.Fatalf("refusal lacks retained native evidence")
			}
		})
	}
}
