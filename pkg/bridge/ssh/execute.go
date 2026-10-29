package ssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/core"
	gossh "golang.org/x/crypto/ssh"
)

type byteCounter struct {
	writer io.Writer
	n      atomic.Int64
}

func (counter *byteCounter) Write(content []byte) (int, error) {
	n, err := counter.writer.Write(content)
	counter.n.Add(int64(n))
	return n, err
}

func (counter *byteCounter) count() int64 { return counter.n.Load() }

type recordedInput struct {
	reader   io.Reader
	mu       sync.Mutex
	n        int64
	data     bytes.Buffer
	overflow bool
}

func (input *recordedInput) Read(content []byte) (int, error) {
	n, err := input.reader.Read(content)
	if n > 0 {
		input.mu.Lock()
		remaining := maxRecordedInputBytes - input.data.Len()
		if n > remaining {
			input.n += int64(n)
			input.data.Reset()
			input.overflow = true
			input.mu.Unlock()
			return n, fmt.Errorf("SSH stdin exceeds %d bytes", maxRecordedInputBytes)
		}
		_, _ = input.data.Write(content[:n])
		input.n += int64(n)
		input.mu.Unlock()
	}
	return n, err
}

func (input *recordedInput) record(retainedRaw bool) (int64, string, string, []byte, bool) {
	input.mu.Lock()
	count := input.n
	content := bytes.Clone(input.data.Bytes())
	overflow := input.overflow
	input.mu.Unlock()
	if safeStructuredText(content) {
		return count, string(content), "utf-8", content, overflow
	}
	// Without the raw log the bytes are retained nowhere, so the note must not
	// point at an artifact this run did not write.
	note := fmt.Sprintf("[binary input omitted; %d bytes not retained]", count)
	if retainedRaw {
		note = fmt.Sprintf("[binary input omitted; %d bytes retained in ssh_raw.log]", count)
	}
	return count, note, "binary-omitted", content, overflow
}

func safeStructuredText(content []byte) bool {
	if !utf8.Valid(content) {
		return false
	}
	for _, value := range string(content) {
		if unicode.IsControl(value) && value != '\t' && value != '\n' && value != '\r' {
			return false
		}
	}
	return true
}

// execute is used by direct execution callers; the SSH server records after
// sending completion and closing the channel through executeRecord instead.
func (session *bridgeSession) execute(ctx context.Context, channel gossh.Channel, prepared Prepared, audit requestAudit) int {
	if audit.timings == nil {
		ctx, audit.timings = core.WithCommandTimings(ctx)
		core.MarkCommandTiming(ctx, "bridge.received")
	}
	exitCode, record, raw := session.executeRecord(ctx, channel, prepared, audit)
	if record != nil {
		session.writeTimedRecord(*record, raw, audit)
	}
	return exitCode
}

func (session *bridgeSession) executeRecord(ctx context.Context, channel gossh.Channel, prepared Prepared, audit requestAudit) (int, *toolCallRecord, rawSSHRecord) {
	started := time.Now()
	stdin := &recordedInput{reader: channel}
	stdout := &byteCounter{writer: channel}
	stderr := &byteCounter{writer: channel.Stderr()}
	var result core.CommandResult
	var err error
	core.MarkCommandTiming(ctx, "bridge.dispatch")
	finishBackend := core.TimeCommandStage(ctx, "bridge.backend")
	if prepared.Action == DrainOnly {
		finishDrain := core.TimeCommandStage(ctx, "bridge.drain_input")
		_, err = io.Copy(io.Discard, stdin)
		finishDrain()
		if err == nil {
			core.MarkCommandTiming(ctx, "command.completed")
		}
	} else {
		result, err = session.sandbox.ExecStream(ctx, prepared.Command, stdin, stdout, stderr)
	}
	finishBackend()
	core.MarkCommandTiming(ctx, "backend.completed")

	exitCode := result.ExitCode
	status, message := "completed", ""
	if err != nil {
		exitCode = 255
		status, message = "failed", "sandbox execution failed"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status, message = "canceled", "session canceled"
		}
	}
	if exitCode < 0 || exitCode > 255 {
		exitCode = 255
	}
	stdinBytes, stdinContent, stdinEncoding, rawStdin, stdinOverflow := stdin.record(session.audit.retainsRaw())
	if stdinOverflow {
		session.audit.latch(fmt.Errorf("retain SSH stdin: input exceeds %d bytes", maxRecordedInputBytes))
		return exitCode, nil, rawSSHRecord{}
	}
	record := &toolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: prepared.OperationClass, Path: prepared.Command.Path, Workdir: prepared.Command.Dir,
		CommandHash: commandHash(prepared.HashInput),
		Command:     prepared.Display, WorkspaceHome: prepared.WorkspaceHome, Environment: prepared.Environment,
		Argv:  append([]string{prepared.Command.Path}, prepared.Command.Args...),
		Stdin: stdinContent, StdinEncoding: stdinEncoding,
		StdinBytes: stdinBytes, StdoutBytes: stdout.count(), StderrBytes: stderr.count(),
		ExitCode: exitCode, DurationMS: time.Since(started).Milliseconds(), Status: status, Error: message,
		RequestType: audit.requestType, WantReply: audit.wantReply,
	}
	return exitCode, record, rawRecord(audit, stdinBytes, rawStdin, status)
}

func (session *bridgeSession) logRejected(audit requestAudit, kind string) {
	session.logRequestFailure(audit, kind, "rejected", "invalid remote command")
}

// logRequestFailure records a request that never reached the sandbox. The kind
// is whatever decoding established before the failure, so a refused file sync
// is not filed as an agent command; kindUnknown marks a payload that never
// decoded far enough to classify.
func (session *bridgeSession) logRequestFailure(audit requestAudit, kind, status, message string) {
	session.writeTimedRecord(toolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: kind, CommandHash: commandHash(audit.remoteCommand),
		StdinEncoding: "utf-8",
		// The request never ran, so the record must not carry the exit code of
		// a successful command.
		ExitCode: session.dialect.Policy().RefusedExitCode,
		Status:   status, Error: message,
		RequestType: audit.requestType, WantReply: audit.wantReply,
	}, rawRecord(audit, 0, nil, status), audit)
}

func rawRecord(audit requestAudit, stdinBytes int64, stdin []byte, status string) rawSSHRecord {
	return rawSSHRecord{
		RequestType: audit.requestType, WantReply: audit.wantReply,
		WireCommand: audit.remoteCommand, Payload: bytes.Clone(audit.payload), PayloadBytes: int64(len(audit.payload)),
		Stdin: bytes.Clone(stdin), StdinBytes: stdinBytes, Status: status,
	}
}

func (session *bridgeSession) writeTimedRecord(record toolCallRecord, raw rawSSHRecord, audit requestAudit) {
	if audit.timings != nil {
		timing := audit.timings.Snapshot()
		record.Timing = &timing
	}
	session.writeRecord(record, raw)
}

func (session *bridgeSession) writeRecord(record toolCallRecord, raw rawSSHRecord) {
	record.SandboxID = session.sandboxID
	raw.SandboxID = session.sandboxID
	record.RunID = session.sandbox.RunID()
	record.TaskID = session.sandbox.TaskID()
	raw.RunID = session.sandbox.RunID()
	raw.TaskID = session.sandbox.TaskID()
	raw.ContainerID = session.sandbox.ContainerID()
	session.audit.enqueue(record, raw)
}

func commandHash(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}
