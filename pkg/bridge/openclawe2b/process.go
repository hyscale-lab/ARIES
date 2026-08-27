package openclawe2b

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	arsandbox "github.com/hyscale-lab/aries/pkg/sandbox"
)

const maxProcessBody = 1 << 20

type processSandbox interface {
	runner.Sandbox
	ExecProcessStream(context.Context, core.Command, io.Writer, io.Writer, func(arsandbox.ProcessRef) error) (core.CommandResult, error)
	SendProcessSignal(context.Context, arsandbox.ProcessRef, string) error
	TerminateProcess(context.Context, arsandbox.ProcessRef) error
}

type processKey struct {
	sandboxID string
	pid       int
}

type activeProcess struct {
	key          processKey
	generation   uint64
	registration *registration
	sandbox      processSandbox
	ref          arsandbox.ProcessRef
}

type processStartRequest struct {
	Process *struct {
		Cmd  string            `json:"cmd"`
		Args []string          `json:"args"`
		Cwd  string            `json:"cwd"`
		Envs map[string]string `json:"envs"`
	} `json:"process"`
}

type processSignalRequest struct {
	Process *struct {
		PID int `json:"pid"`
	} `json:"process"`
	Signal string `json:"signal"`
}

type processSignalResponse struct {
	OK bool `json:"ok"`
}

type processEvent struct {
	Event struct {
		Start *processStartEvent `json:"start,omitempty"`
		Data  *processDataEvent  `json:"data,omitempty"`
		End   *processEndEvent   `json:"end,omitempty"`
	} `json:"event"`
}

type processStartEvent struct {
	PID int `json:"pid"`
}

type processDataEvent struct {
	Stdout []byte `json:"stdout,omitempty"`
	Stderr []byte `json:"stderr,omitempty"`
}

type processEndEvent struct {
	ExitCode int     `json:"exitCode"`
	Error    *string `json:"error"`
}

type processEventStream struct {
	mu      sync.Mutex
	encoder *json.Encoder
	flusher http.Flusher
	started bool
	ended   bool
}

func (server *Server) serveProcessStart(response http.ResponseWriter, request *http.Request) {
	registration := server.registrationForAdmittedRequest(request.Header.Get(sandboxIDHeader))
	sandbox, ok := registration.sandbox.(processSandbox)
	if !ok {
		writeJSONError(response, http.StatusBadRequest, "sandbox does not support attached process execution")
		return
	}
	command, err := decodeProcessStart(response, request)
	if err != nil {
		writeJSONError(response, http.StatusBadRequest, err.Error())
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeJSONError(response, http.StatusInternalServerError, "streaming response is unavailable")
		return
	}
	response.Header().Set("Content-Type", "application/x-ndjson")
	stream := &processEventStream{encoder: json.NewEncoder(response), flusher: flusher}
	var tracked *activeProcess
	result, execErr := sandbox.ExecProcessStream(request.Context(), command,
		&processDataWriter{stream: stream, stdout: true},
		&processDataWriter{stream: stream},
		func(ref arsandbox.ProcessRef) error {
			process, err := server.registerProcess(registration, sandbox, ref)
			if err != nil {
				return err
			}
			tracked = process
			if err := stream.start(ref.PID); err != nil {
				server.removeProcess(process)
				tracked = nil
				return err
			}
			return nil
		},
	)
	if tracked != nil {
		server.removeProcess(tracked)
	}
	if !stream.hasStarted() {
		if execErr == nil {
			execErr = errors.New("process exited without a start event")
		}
		writeJSONError(response, http.StatusBadRequest, execErr.Error())
		return
	}
	_ = stream.end(result.ExitCode, execErr)
}

func (server *Server) registerProcess(registration *registration, sandbox processSandbox, ref arsandbox.ProcessRef) (*activeProcess, error) {
	if ref.PID <= 1 {
		return nil, errors.New("process start returned an invalid PID")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if current := server.registrations[registration.sandboxID]; current != registration || registration.state != registrationActive {
		return nil, errors.New("sandbox grant is being revoked")
	}
	key := processKey{sandboxID: registration.sandboxID, pid: ref.PID}
	if _, exists := server.processes[key]; exists {
		return nil, fmt.Errorf("process PID %d is already active in sandbox", ref.PID)
	}
	server.nextProcess++
	process := &activeProcess{key: key, generation: server.nextProcess, registration: registration, sandbox: sandbox, ref: ref}
	server.processes[key] = process
	return process, nil
}

func (server *Server) removeProcess(process *activeProcess) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.processes[process.key] == process {
		delete(server.processes, process.key)
	}
}

func (server *Server) processesFor(registration *registration) []*activeProcess {
	server.mu.Lock()
	defer server.mu.Unlock()
	processes := make([]*activeProcess, 0)
	for _, process := range server.processes {
		if process.registration == registration {
			processes = append(processes, process)
		}
	}
	return processes
}

func (server *Server) processCountLocked(sandboxID string) int {
	count := 0
	for key := range server.processes {
		if key.sandboxID == sandboxID {
			count++
		}
	}
	return count
}

func (server *Server) serveProcessSendSignal(response http.ResponseWriter, request *http.Request) {
	registration := server.registrationForAdmittedRequest(request.Header.Get(sandboxIDHeader))
	payload, err := decodeProcessSignal(response, request)
	if err != nil {
		writeJSONError(response, http.StatusBadRequest, err.Error())
		return
	}
	key := processKey{sandboxID: registration.sandboxID, pid: payload.Process.PID}
	server.mu.Lock()
	process := server.processes[key]
	server.mu.Unlock()
	if process == nil || process.registration != registration {
		writeJSONError(response, http.StatusNotFound, "process is not active in this sandbox")
		return
	}
	if err := process.sandbox.SendProcessSignal(request.Context(), process.ref, payload.Signal); err != nil {
		writeJSONError(response, http.StatusConflict, err.Error())
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(processSignalResponse{OK: true})
}

func decodeProcessSignal(response http.ResponseWriter, request *http.Request) (processSignalRequest, error) {
	request.Body = http.MaxBytesReader(response, request.Body, maxProcessBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var payload processSignalRequest
	if err := decoder.Decode(&payload); err != nil {
		return payload, fmt.Errorf("decode Process.SendSignal request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return payload, fmt.Errorf("decode Process.SendSignal request: %w", err)
	}
	if payload.Process == nil || payload.Process.PID <= 1 {
		return payload, errors.New("Process.SendSignal requires a positive process.pid")
	}
	if payload.Signal != "SIGNAL_SIGTERM" && payload.Signal != "SIGNAL_SIGKILL" {
		return payload, fmt.Errorf("unsupported process signal %q", payload.Signal)
	}
	return payload, nil
}

func (server *Server) registrationForAdmittedRequest(sandboxID string) *registration {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.registrations[sandboxID]
}

func decodeProcessStart(response http.ResponseWriter, request *http.Request) (core.Command, error) {
	request.Body = http.MaxBytesReader(response, request.Body, maxProcessBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var payload processStartRequest
	if err := decoder.Decode(&payload); err != nil {
		return core.Command{}, fmt.Errorf("decode Process.Start request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return core.Command{}, fmt.Errorf("decode Process.Start request: %w", err)
	}
	if payload.Process == nil || payload.Process.Cmd == "" {
		return core.Command{}, errors.New("Process.Start requires process.cmd")
	}
	return core.Command{
		Path: payload.Process.Cmd,
		Args: append([]string(nil), payload.Process.Args...),
		Dir:  payload.Process.Cwd,
		Env:  cloneStrings(payload.Process.Envs),
	}, nil
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func (stream *processEventStream) start(pid int) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.started || stream.ended {
		return errors.New("duplicate process start event")
	}
	event := processEvent{}
	event.Event.Start = &processStartEvent{PID: pid}
	if err := stream.encoder.Encode(event); err != nil {
		return err
	}
	stream.flusher.Flush()
	stream.started = true
	return nil
}

func (stream *processEventStream) data(stdout bool, content []byte) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if !stream.started || stream.ended {
		return errors.New("process data outside active stream")
	}
	event := processEvent{}
	event.Event.Data = &processDataEvent{}
	if stdout {
		event.Event.Data.Stdout = append([]byte(nil), content...)
	} else {
		event.Event.Data.Stderr = append([]byte(nil), content...)
	}
	if err := stream.encoder.Encode(event); err != nil {
		return err
	}
	stream.flusher.Flush()
	return nil
}

func (stream *processEventStream) end(exitCode int, execErr error) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if !stream.started || stream.ended {
		return errors.New("invalid process end event")
	}
	event := processEvent{}
	event.Event.End = &processEndEvent{ExitCode: exitCode}
	if execErr != nil {
		message := execErr.Error()
		event.Event.End.Error = &message
	}
	if err := stream.encoder.Encode(event); err != nil {
		return err
	}
	stream.flusher.Flush()
	stream.ended = true
	return nil
}

func (stream *processEventStream) hasStarted() bool {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.started
}

type processDataWriter struct {
	stream *processEventStream
	stdout bool
}

func (writer *processDataWriter) Write(content []byte) (int, error) {
	if len(content) == 0 {
		return 0, nil
	}
	if err := writer.stream.data(writer.stdout, content); err != nil {
		return 0, err
	}
	return len(content), nil
}

func writeJSONError(response http.ResponseWriter, status int, message string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": message})
}
