package ssh_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	bridgessh "github.com/hyscale-lab/aries/pkg/bridge/ssh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/dsh"
	"github.com/hyscale-lab/aries/pkg/bridge/ssh/internal/testfixture"
	sshclient "github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/client"
	"github.com/hyscale-lab/aries/pkg/core"
	gossh "golang.org/x/crypto/ssh"
)

func TestDSHStreamLocalRefusesExecutionAfterAuditFailure(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := bridgessh.New(bridgessh.Options{
		Dialect: dsh.Dialect{}, HostSigner: signer, SandboxID: "dsh-audit-failure", OutputDir: t.TempDir(),
		ResolveListen: func(context.Context) (core.BridgeListen, error) {
			return core.BridgeListen{BindHost: "127.0.0.1"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wantStopErr error
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := manager.Stop(ctx); !errors.Is(err, wantStopErr) {
			t.Fatalf("Stop() = %v, want original audit error %v", err, wantStopErr)
		}
	})
	executor := &contractExecutor{}
	endpoint, err := manager.StartTarget(context.Background(), executor)
	if err != nil {
		t.Fatal(err)
	}
	auditErr := errors.New("injected DSH audit failure")
	bridgessh.LatchAuditErrorForTest(manager, auditErr)
	wantStopErr = auditErr
	client, err := gossh.Dial("tcp", endpoint.Address, &gossh.ClientConfig{
		User: endpoint.Username, HostKeyCallback: gossh.InsecureIgnoreHostKey(), Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	payload := gossh.Marshal(struct {
		Socket, Reserved string
		Port             uint32
	}{Socket: "/tmp/dsh-ssh-a1b2/stream-1"})
	channel, _, err := client.OpenChannel("direct-streamlocal@openssh.com", payload)
	if channel != nil {
		_ = channel.Close()
	}
	var rejection *gossh.OpenChannelError
	if !errors.As(err, &rejection) || rejection.Reason != gossh.Prohibited || rejection.Message != "bridge audit failed" {
		t.Fatalf("stream-local request = %v, want audit rejection", err)
	}
	if calls, _ := executor.snapshot(); len(calls) != 0 {
		t.Fatalf("ExecStream called after audit failure: %#v", calls)
	}
	if err := manager.Stop(context.Background()); !errors.Is(err, auditErr) {
		t.Fatalf("original audit error lost: %v", err)
	}
}

func TestDSHOversizedStreamingEvidenceMatchesRawRetention(t *testing.T) {
	input := bytes.Repeat([]byte{'p'}, bridgekit.MaxRecordedInputBytes+123)
	copy(input[bridgekit.MaxRecordedInputBytes:], bytes.Repeat([]byte{'s'}, 123))
	for _, retainRaw := range []bool{true, false} {
		t.Run(fmt.Sprintf("raw=%t", retainRaw), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			output := t.TempDir()
			server := testfixture.New(t, bridgessh.Options{Dialect: dsh.Dialect{}, OutputDir: output, OmitRawLog: !retainRaw})
			executor := &contractExecutor{}
			endpoint, err := server.StartTarget(ctx, executor)
			if err != nil {
				t.Fatal(err)
			}
			code, err := sshclient.Run(ctx, sshclient.Config{Address: endpoint.Address, User: endpoint.Username}, dsh.HelperCommand,
				io.NopCloser(bytes.NewReader(input)), io.Discard, io.Discard)
			if code != 7 || err != nil {
				t.Fatalf("stream execution: code=%d, error=%v", code, err)
			}
			calls, forwarded := executor.snapshot()
			if len(calls) != 1 || !calls[0].StreamProtocol || !bytes.Equal(forwarded, input) {
				t.Fatal("stream input was limited or changed")
			}
			if err := server.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			records := testfixture.ReadToolCalls(t, filepath.Join(output, "tool-calls.jsonl"))
			if len(records) != 1 {
				t.Fatalf("structured records = %d, want one", len(records))
			}
			record := records[0]
			wantNote := fmt.Sprintf("[stream input truncated; %d total bytes; input bytes not retained]", len(input))
			if retainRaw {
				wantNote = fmt.Sprintf("[stream input truncated; %d total bytes; %d prefix bytes retained in ssh_raw.log]", len(input), bridgekit.MaxRecordedInputBytes)
			}
			if record["stdin"] != wantNote || record["stdin_encoding"] != "truncated-prefix" || record["stdin_bytes"] != float64(len(input)) || record["stdout_bytes"] != float64(len(input)) || record["status"] != "completed" {
				t.Fatalf("inaccurate streaming evidence: %#v", record)
			}
			raw, err := os.ReadFile(filepath.Join(output, "ssh_raw.log"))
			if !retainRaw {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("raw log exists or could not be inspected: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			marker := fmt.Sprintf("stdin_bytes=%d\nstdin_retained_bytes=%d\nstdin_truncated=true\nstdin=", len(input), bridgekit.MaxRecordedInputBytes)
			_, prefix, found := bytes.Cut(raw, []byte(marker))
			prefix, ended := bytes.CutSuffix(prefix, []byte("\n--- ARIES SSH CALL END ---\n"))
			if !found || !ended || !bytes.Equal(prefix, input[:bridgekit.MaxRecordedInputBytes]) {
				t.Fatal("raw input prefix or byte counts differ from structured evidence")
			}
		})
	}
}
