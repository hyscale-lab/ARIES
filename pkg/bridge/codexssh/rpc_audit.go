package codexssh

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/sshbridge"
)

const maxRPCFrameBytes = 16 << 20
const maxActiveRPCProcesses = 4096

// rpcAudit observes the pinned exec-server JSONL protocol. Times are bridge
// observations, not kernel process times. It never edits or buffers forwarding.
// Environment values and output contents are deliberately not copied to records.
type rpcAudit struct {
	mu            sync.Mutex
	input, output []byte
	processes     map[string]*rpcProcess
	requests      map[string]string
	activeBytes   int
	emit          func(sshbridge.ToolCallRecord)
	latch         func(error)
	now           func() time.Time
	failed        bool
}
type rpcProcess struct {
	record  sshbridge.ToolCallRecord
	started time.Time
	bytes   int
	closed  bool
}

func newRPCAudit(emit func(sshbridge.ToolCallRecord), latch func(error)) *rpcAudit {
	return &rpcAudit{processes: make(map[string]*rpcProcess), requests: make(map[string]string), emit: emit, latch: latch, now: time.Now}
}
func (a *rpcAudit) fail(message string) {
	if !a.failed {
		a.failed = true
		a.latch(errors.New("Codex RPC audit: " + message))
	}
}
func (a *rpcAudit) feed(input bool, data []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return
	}
	pending := &a.output
	if input {
		pending = &a.input
	}
	for len(data) > 0 {
		n := bytes.IndexByte(data, '\n')
		end := n
		if n < 0 {
			end = len(data)
		}
		if len(*pending)+end > maxRPCFrameBytes {
			a.fail("frame exceeds observation limit")
			*pending = nil
			return
		}
		*pending = append(*pending, data[:end]...)
		if n < 0 {
			return
		}
		a.frame(input, *pending)
		*pending = (*pending)[:0]
		if a.failed {
			return
		}
		data = data[n+1:]
	}
}
func (a *rpcAudit) frame(input bool, line []byte) {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &frame) != nil {
		a.fail("malformed JSON frame")
		return
	}
	if input {
		if frame.Method != "process/start" {
			return
		}
		var p struct {
			ProcessID string   `json:"processId"`
			Argv      []string `json:"argv"`
			Cwd       string   `json:"cwd"`
			Metadata  struct {
				ThreadID   string `json:"threadId"`
				ToolCallID string `json:"toolCallId"`
			} `json:"metadata"`
		}
		if json.Unmarshal(frame.Params, &p) != nil || p.ProcessID == "" || len(p.Argv) == 0 || len(frame.ID) == 0 {
			a.fail("invalid process/start frame")
			return
		}
		request := string(frame.ID)
		if a.processes[p.ProcessID] != nil || a.requests[request] != "" {
			a.fail("duplicate active process or request ID")
			return
		}
		if len(a.processes) >= maxActiveRPCProcesses || a.activeBytes+len(line) > maxRPCFrameBytes {
			a.fail("active process observation limit exceeded")
			return
		}
		now := a.now()
		argvJSON, _ := json.Marshal(p.Argv)
		a.processes[p.ProcessID] = &rpcProcess{started: now, bytes: len(line), record: sshbridge.ToolCallRecord{
			StartedAt: now.UTC().Format(time.RFC3339Nano), RequestID: request, ProcessID: p.ProcessID, ThreadID: p.Metadata.ThreadID, ToolCallID: p.Metadata.ToolCallID,
			OperationClass: "exec", Path: p.Argv[0], Argv: p.Argv, Workdir: p.Cwd, CommandHash: commandHash(string(argvJSON)), StdinEncoding: "utf-8", ExitCode: -1, RequestType: "process/start",
		}}
		a.requests[request] = p.ProcessID
		a.activeBytes += len(line)
		return
	}
	if frame.Method == "process/output" || frame.Method == "process/exited" || frame.Method == "process/closed" {
		var p struct {
			ProcessID string `json:"processId"`
			Stream    string `json:"stream"`
			Chunk     string `json:"chunk"`
			ExitCode  *int   `json:"exitCode"`
		}
		if json.Unmarshal(frame.Params, &p) != nil {
			a.fail("invalid process notification")
			return
		}
		process := a.processes[p.ProcessID]
		if process == nil {
			return
		}
		if frame.Method == "process/exited" {
			if p.ExitCode == nil {
				a.fail("exit notification lacks exit code")
				return
			}
			if process.record.FinishedAt != "" {
				a.fail("duplicate process exit")
				return
			}
			process.record.ExitCode = *p.ExitCode
			process.record.Status = "completed"
			now := a.now()
			process.record.FinishedAt = now.UTC().Format(time.RFC3339Nano)
			process.record.DurationMS = now.Sub(process.started).Milliseconds()
			if process.closed {
				a.complete(p.ProcessID, "completed", true)
			}
			return
		}
		if frame.Method == "process/closed" {
			process.closed = true
			if process.record.FinishedAt != "" {
				a.complete(p.ProcessID, "completed", true)
			}
			return
		}
		chunk, err := base64.StdEncoding.DecodeString(p.Chunk)
		if err != nil {
			a.fail("invalid process output encoding")
			return
		}
		switch p.Stream {
		case "stdout", "pty":
			process.record.StdoutBytes += int64(len(chunk))
		case "stderr":
			process.record.StderrBytes += int64(len(chunk))
		default:
			a.fail("invalid process output stream")
		}
		return
	}
	if len(frame.Error) > 0 && string(frame.Error) != "null" {
		if id := a.requests[string(frame.ID)]; id != "" {
			a.complete(id, "start_failed", true)
		}
	}
}
func (a *rpcAudit) complete(id, status string, confirmed bool) {
	process := a.processes[id]
	now := a.now()
	if process.record.FinishedAt == "" {
		process.record.Status = status
		process.record.DurationMS = now.Sub(process.started).Milliseconds()
		if confirmed {
			process.record.FinishedAt = now.UTC().Format(time.RFC3339Nano)
		}
	}
	a.emit(process.record)
	delete(a.requests, process.record.RequestID)
	delete(a.processes, id)
	a.activeBytes -= process.bytes
}
func (a *rpcAudit) finish(canceled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.input) > 0 || len(a.output) > 0 {
		a.fail("unterminated JSON frame")
	}
	status := "incomplete"
	if canceled {
		status = "canceled"
	}
	for id := range a.processes {
		a.complete(id, status, false)
	}
}

type rpcAuditReader struct {
	reader io.Reader
	audit  *rpcAudit
}

func (r rpcAuditReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.audit.feed(true, p[:n])
	return n, err
}

type rpcAuditWriter struct {
	writer io.Writer
	audit  *rpcAudit
}

func (w rpcAuditWriter) Write(p []byte) (int, error) {
	w.audit.feed(false, p)
	return w.writer.Write(p)
}
