package ssh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	gossh "golang.org/x/crypto/ssh"
)

type latencyDialect struct{ testDialect }

func (d latencyDialect) Prepare(command, workdir string) (Prepared, *Refusal) {
	time.Sleep(2 * time.Millisecond)
	return d.testDialect.Prepare(command, workdir)
}

type latencySandbox struct {
	contractSandbox
	failure error
}

func (s *latencySandbox) ExecStream(ctx context.Context, _ core.Command, _ io.Reader, _, _ io.Writer) (core.CommandResult, error) {
	finishRestore := core.TimeCommandStage(ctx, "restore.prepare_mounts")
	time.Sleep(2 * time.Millisecond)
	finishRestore()
	core.MarkCommandTiming(ctx, "command.started")
	finishRun := core.TimeCommandStage(ctx, "command.wait")
	time.Sleep(2 * time.Millisecond)
	finishRun()
	if s.failure != nil {
		return core.CommandResult{ExitCode: -1}, s.failure
	}
	core.MarkCommandTiming(ctx, "command.exited")
	core.MarkCommandTiming(ctx, "command.completed")
	finishCheckpoint := core.TimeCommandStage(ctx, "checkpoint.total")
	time.Sleep(5 * time.Millisecond)
	finishCheckpoint()
	return core.CommandResult{ExitCode: 7}, nil
}

type latencyChannel struct {
	stubSSHChannel
	responseErr, closeErr error
}

func (c *latencyChannel) SendRequest(name string, _ bool, _ []byte) (bool, error) {
	if name != "exit-status" {
		return false, errors.New("unexpected response")
	}
	time.Sleep(2 * time.Millisecond)
	return false, c.responseErr
}
func (c *latencyChannel) Close() error { time.Sleep(2 * time.Millisecond); return c.closeErr }

func TestBridgeLatencySeparatesCommandCompletionFromCheckpointAndResponse(t *testing.T) {
	structured, content := memoryAuditFile()
	sandbox := &latencySandbox{}
	session := &bridgeSession{dialect: latencyDialect{}, sandbox: sandbox, audit: newAuditWriter(structured, nil), replyRequest: func(*gossh.Request, bool) error { time.Sleep(2 * time.Millisecond); return nil }}
	wire := encodeCanonicalTokens([]string{remoteShell, "-c", "true"})
	requests := make(chan *gossh.Request, 1)
	requests <- &gossh.Request{Type: "exec", WantReply: true, Payload: gossh.Marshal(struct{ Command string }{wire})}
	close(requests)
	session.handleSession(context.Background(), &latencyChannel{}, requests)
	if err := session.closeAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	var record toolCallRecord
	if err := json.Unmarshal(bytes.TrimSpace(content.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record.ExitCode != 7 || record.Timing == nil {
		t.Fatalf("lost command result/timing: %#v", record)
	}
	timing := record.Timing
	ordered := []string{"bridge.received", "bridge.prepared", "bridge.dispatch", "command.started", "command.exited", "command.completed", "backend.completed", "bridge.response_sent", "bridge.channel_closed"}
	for i, name := range ordered {
		point, ok := timing.Milestones[name]
		if !ok {
			t.Fatalf("missing %s: %#v", name, timing)
		}
		if i > 0 && point.OffsetMS < timing.Milestones[ordered[i-1]].OffsetMS {
			t.Fatalf("milestones out of order: %#v", timing.Milestones)
		}
	}
	commandDone := timing.Milestones["command.completed"].OffsetMS
	if timing.Milestones["backend.completed"].OffsetMS-commandDone < 5 {
		t.Fatalf("checkpoint hidden in command duration: %#v", timing)
	}
	stageByName := map[string]core.CommandTimingStage{}
	for _, stage := range timing.Stages {
		stageByName[stage.Name] = stage
	}
	for _, name := range []string{"bridge.prepare", "bridge.accept_reply", "restore.prepare_mounts", "command.wait", "checkpoint.total", "bridge.send_exit_status", "bridge.close_channel"} {
		if stage, ok := stageByName[name]; !ok || stage.DurationMS < 1 {
			t.Fatalf("missing material delay %s: %#v", name, timing.Stages)
		}
	}
	if stageByName["checkpoint.total"].OffsetMS < commandDone {
		t.Fatal("checkpoint predates command completion")
	}
	completion, err := time.Parse(time.RFC3339Nano, record.Timestamp)
	if err != nil {
		t.Fatal(err)
	}
	if completion.Before(timing.ReceivedAt) || completion.Before(timing.Milestones["bridge.channel_closed"].At) {
		t.Fatal("legacy timestamp no longer audit completion time")
	}
	if _, ok := timing.Milestones["harness.response_received"]; ok {
		t.Fatal("bridge invented remote receipt")
	}
}

func TestBridgeLatencyPreservesFailuresWithoutInventingCompletion(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		execErr, responseErr, closeErr error
	}{
		{name: "command transport", execErr: context.Canceled},
		{name: "response transport", responseErr: errors.New("send failed")},
		{name: "channel close", closeErr: errors.New("close failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			structured, content := memoryAuditFile()
			sandbox := &latencySandbox{failure: tc.execErr}
			session := &bridgeSession{dialect: testDialect{}, sandbox: sandbox, audit: newAuditWriter(structured, nil), replyRequest: func(*gossh.Request, bool) error { return nil }}
			wire := encodeCanonicalTokens([]string{remoteShell, "-c", "true"})
			requests := make(chan *gossh.Request, 1)
			requests <- &gossh.Request{Type: "exec", WantReply: true, Payload: gossh.Marshal(struct{ Command string }{wire})}
			close(requests)
			session.handleSession(context.Background(), &latencyChannel{responseErr: tc.responseErr, closeErr: tc.closeErr}, requests)
			if err := session.closeAudit(context.Background()); err != nil {
				t.Fatal(err)
			}
			var record toolCallRecord
			if err := json.Unmarshal(bytes.TrimSpace(content.Bytes()), &record); err != nil {
				t.Fatal(err)
			}
			if tc.execErr != nil {
				if record.Status != "canceled" || record.ExitCode != 255 {
					t.Fatalf("execution failure lost: %#v", record)
				}
				if _, ok := record.Timing.Milestones["command.completed"]; ok {
					t.Fatal("canceled command reported complete")
				}
			}
			if tc.responseErr != nil {
				if _, ok := record.Timing.Milestones["bridge.response_sent"]; ok {
					t.Fatal("failed send reported successful")
				}
				if !strings.Contains(record.ResponseError, "exit-status") {
					t.Fatalf("response failure omitted: %#v", record)
				}
			}
			if tc.closeErr != nil {
				if _, ok := record.Timing.Milestones["bridge.channel_closed"]; ok {
					t.Fatal("failed close reported successful")
				}
				if !strings.Contains(record.ResponseError, "close") {
					t.Fatalf("close failure omitted: %#v", record)
				}
			}
		})
	}
}

func TestBridgeLatencyLeavesUnobservedCommandCompletionAbsent(t *testing.T) {
	for _, action := range []Action{Execute, DrainOnly} {
		name := "execute"
		if action == DrainOnly {
			name = "drain"
		}
		t.Run(name, func(t *testing.T) {
			structured, content := memoryAuditFile()
			session := &bridgeSession{dialect: testDialect{}, sandbox: &contractSandbox{}, audit: newAuditWriter(structured, nil)}
			prepared := Prepared{Action: action, Command: core.Command{Path: "/bin/true"}}
			if exit := session.execute(context.Background(), &stubSSHChannel{}, prepared, requestAudit{}); exit != 0 {
				t.Fatalf("exit=%d", exit)
			}
			if err := session.closeAudit(context.Background()); err != nil {
				t.Fatal(err)
			}
			var record toolCallRecord
			if err := json.Unmarshal(bytes.TrimSpace(content.Bytes()), &record); err != nil {
				t.Fatal(err)
			}
			if _, ok := record.Timing.Milestones["backend.completed"]; !ok {
				t.Fatal("backend completion missing")
			}
			_, commandDone := record.Timing.Milestones["command.completed"]
			if commandDone != (action == DrainOnly) {
				t.Fatalf("command completion fabricated or drain EOF lost: action=%v timing=%#v", action, record.Timing)
			}
		})
	}
}
