//go:build linux

package execsupervisor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const agentStagePrefix = "/.aries-agent-"

type brokerConfig struct {
	stage          string
	credential     *syscall.Credential
	passwd, groups []byte
	workerArgs     []string // Only subprocess fixtures override --worker.
}

// RunBroker owns one complete agent lifetime inside a task container. It is
// started before SSH access and never reexecutes itself from its staged path.
func RunBroker(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) (int, error) {
	if len(args) != 5 || args[0] != "--broker" || args[1] != "--stage-dir" || args[3] != "--user" || !validAgentStage(args[2]) || args[4] == "" {
		return supervisorFailureCode, errors.New("agent broker requires --broker --stage-dir STAGE --user USER")
	}
	if os.Geteuid() != 0 {
		return supervisorFailureCode, errors.New("agent broker must run as root")
	}
	passwd, err := os.ReadFile("/etc/passwd")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return supervisorFailureCode, errors.New("read agent task users failed")
	}
	groups, err := os.ReadFile("/etc/group")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return supervisorFailureCode, errors.New("read agent task groups failed")
	}
	credential, err := resolveSupervisorCredential(args[4], passwd, groups)
	if err != nil {
		return supervisorFailureCode, err
	}
	return runBroker(ctx, brokerConfig{stage: args[2], credential: credential, passwd: passwd, groups: groups}, stdin, stdout, stderr)
}

func validAgentStage(stage string) bool {
	if !strings.HasPrefix(stage, agentStagePrefix) || len(stage) != len(agentStagePrefix)+32 || filepath.Clean(stage) != stage {
		return false
	}
	for _, r := range stage[len(agentStagePrefix):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

type brokerEvent struct {
	worker  *brokerWorker
	type_   string
	message workerMessage
	err     error
}

type brokerInput struct {
	data []byte
	eof  bool
}

type brokerWorker struct {
	id             uint64
	command        *exec.Cmd
	control        *os.File
	stdin          *os.File
	stdout, stderr *os.File
	input          chan brokerInput
	outAck, errAck chan struct{}
	outPending     atomic.Bool
	errPending     atomic.Bool
	inPending      atomic.Bool
	foregroundDone atomic.Bool
	outputDeadline atomic.Pointer[time.Time]
	inputClosed    bool
	inputBytes     int
	exited, waited bool
	outputDone     int
	stopOnce       sync.Once
	stopRequested  chan struct{}
	foregroundExit chan struct{}
	controlMu      sync.Mutex
	forceTimer     *time.Timer
}

type agentBroker struct {
	config   brokerConfig
	workers  map[uint64]*brokerWorker
	used     map[uint64]struct{}
	events   chan brokerEvent
	requests chan Message
	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	stopMu   sync.Mutex
	stopErr  error
	output   *brokerOutput
}

func runBroker(ctx context.Context, config brokerConfig, stdin *os.File, stdout, stderr io.Writer) (int, error) {
	if stdin == nil || stdout == nil || stderr == nil || !filepath.IsAbs(config.stage) || filepath.Clean(config.stage) != config.stage || config.stage == "/" {
		return supervisorFailureCode, errors.New("agent broker descriptors or stage are invalid")
	}
	runtime.GOMAXPROCS(1)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	release, err := protectSupervisor()
	if err != nil {
		return supervisorFailureCode, err
	}
	defer release()
	nonce, err := readSupervisorNonce(ctx, stdin)
	if err != nil {
		return supervisorFailureCode, err
	}
	b := &agentBroker{config: config, workers: make(map[uint64]*brokerWorker), used: make(map[uint64]struct{}), events: make(chan brokerEvent, 64), requests: make(chan Message, 32), stop: make(chan struct{}), stopped: make(chan struct{})}
	defer close(b.stopped)
	b.output = newBrokerOutput(stdout, func(err error) { b.requestStop(err) })
	b.output.push(Message{Type: "ready", Version: ProtocolVersion})
	go b.readRequests(stdin)
	running := true
	for running {
		select {
		case <-ctx.Done():
			b.requestStop(nil)
		case <-b.stop:
			running = false
		case message := <-b.requests:
			if err := b.handleRequest(message); err != nil {
				b.requestStop(err)
			}
		case event := <-b.events:
			b.handleEvent(event)
		}
	}
	cleanup, done := context.WithTimeout(context.Background(), supervisorGrace+supervisorCleanup+2*time.Second)
	defer done()
	for _, worker := range b.workers {
		if !worker.waited {
			worker.stop()
		}
	}
	for b.liveWorkers() != 0 {
		select {
		case event := <-b.events:
			b.handleEvent(event)
		case <-cleanup.Done():
			return supervisorFailureCode, errors.New("agent workers did not confirm process exit")
		}
	}
	// Every registered Cmd.Wait has finished. Only this goroutine now consumes
	// child statuses, including descendants adopted from a killed worker.
	if err := reapSupervisorChildren(cleanup); err != nil {
		return supervisorFailureCode, err
	}
	for _, worker := range b.workers {
		worker.closeStreams()
	}
	for b.pendingOutputs() != 0 {
		select {
		case event := <-b.events:
			b.handleEvent(event)
		case <-cleanup.Done():
			return supervisorFailureCode, errors.New("agent output pumps did not finish")
		}
	}
	if err := removeSupervisorStage(cleanup, config.stage); err != nil {
		return supervisorFailureCode, err
	}
	b.stopMu.Lock()
	stopErr := b.stopErr
	b.stopMu.Unlock()
	if stopErr != nil {
		b.output.push(Message{Type: "fatal", Error: stopErr.Error()})
	}
	if err := b.output.flush(cleanup); err != nil {
		return supervisorFailureCode, err
	}
	if stopErr != nil {
		return supervisorFailureCode, stopErr
	}
	if err := writeSupervisorMessage(stderr, AgentProofPrefix+nonce+AgentProofSuffix); err != nil {
		return supervisorFailureCode, err
	}
	return 0, nil
}

func (b *agentBroker) requestStop(err error) {
	b.stopMu.Lock()
	if err != nil && b.stopErr == nil {
		b.stopErr = err
	}
	b.stopMu.Unlock()
	b.stopOnce.Do(func() { close(b.stop) })
}

func (b *agentBroker) readRequests(stdin io.Reader) {
	for {
		message, err := ReadMessage(stdin)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				b.requestStop(errors.New("agent broker input protocol failed"))
			} else {
				b.requestStop(nil)
			}
			return
		}
		if message.Type == "stop" {
			b.requestStop(nil)
			continue
		}
		select {
		case b.requests <- message:
		case <-b.stopped:
			return
		}
	}
}

func (b *agentBroker) handleRequest(message Message) error {
	if message.Type == "exec" {
		if _, exists := b.used[message.ID]; exists {
			return errors.New("agent command identity was reused")
		}
		b.used[message.ID] = struct{}{}
		return b.startWorker(message.ID, *message.Exec)
	}
	worker, exists := b.workers[message.ID]
	if !exists {
		if _, retired := b.used[message.ID]; retired && (message.Type == "cancel" || message.Type == "stdin" || message.Type == "stdin_eof" || message.Type == "stdout_ack" || message.Type == "stderr_ack") {
			return nil // A result may cross its final caller-side close/ACK.
		}
		return errors.New("agent protocol references an unknown command")
	}
	switch message.Type {
	case "stdin":
		if worker.foregroundDone.Load() {
			return nil
		}
		if worker.inputClosed || !worker.inPending.CompareAndSwap(false, true) || worker.inputBytes+len(message.Data) > 16<<20 {
			return errors.New("agent input exceeded its window or byte limit")
		}
		worker.inputBytes += len(message.Data)
		worker.input <- brokerInput{data: message.Data}
	case "stdin_eof":
		if worker.foregroundDone.Load() || worker.inputClosed {
			return nil
		}
		if worker.inPending.Load() {
			return errors.New("agent stdin EOF preceded its acknowledgement")
		}
		worker.inputClosed = true
		worker.input <- brokerInput{eof: true}
	case "stdout_ack", "stderr_ack":
		pending, ack := &worker.outPending, worker.outAck
		if message.Type == "stderr_ack" {
			pending, ack = &worker.errPending, worker.errAck
		}
		if pending.CompareAndSwap(true, false) {
			select {
			case ack <- struct{}{}:
			default:
				return errors.New("duplicate agent output acknowledgement")
			}
		} else if !worker.waited {
			select {
			case <-worker.stopRequested:
			default:
				return errors.New("unexpected agent output acknowledgement")
			}
		}
	case "cancel":
		if !worker.waited {
			worker.stop()
		}
	default:
		return errors.New("agent protocol message has the wrong direction")
	}
	return nil
}

func (b *agentBroker) startWorker(id uint64, spec ExecSpec) (returnErr error) {
	credential := b.config.credential
	if spec.User != "" {
		var err error
		credential, err = resolveSupervisorCredential(spec.User, b.config.passwd, b.config.groups)
		if err != nil {
			return err
		}
	}
	var opened []*os.File
	defer func() {
		if returnErr != nil {
			for _, file := range opened {
				_ = file.Close()
			}
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		read, write, err := os.Pipe()
		if err == nil {
			opened = append(opened, read, write)
		}
		return read, write, err
	}
	childIn, input, err := pipe()
	if err != nil {
		return errors.New("create agent stdin pipe failed")
	}
	output, childOut, err := pipe()
	if err != nil {
		return errors.New("create agent stdout pipe failed")
	}
	standardError, childErr, err := pipe()
	if err != nil {
		return errors.New("create agent stderr pipe failed")
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return errors.New("create agent worker control socket failed")
	}
	control, childControl := os.NewFile(uintptr(pair[0]), "agent-parent"), os.NewFile(uintptr(pair[1]), "agent-child")
	opened = append(opened, control, childControl)
	args := b.config.workerArgs
	if len(args) == 0 {
		args = []string{"--worker"}
	}
	command := exec.Command("/proc/self/exe", args...)
	command.Stdin, command.Stdout, command.Stderr = childIn, childOut, childErr
	command.ExtraFiles = []*os.File{childControl}
	if err := command.Start(); err != nil {
		return errors.New("start agent worker failed")
	}
	for _, file := range []*os.File{childIn, childOut, childErr, childControl} {
		_ = file.Close()
	}
	worker := &brokerWorker{id: id, command: command, control: control, stdin: input, stdout: output, stderr: standardError, input: make(chan brokerInput, 1), outAck: make(chan struct{}, 1), errAck: make(chan struct{}, 1), stopRequested: make(chan struct{}), foregroundExit: make(chan struct{})}
	b.workers[id] = worker
	controlDone := make(chan struct{})
	go b.watchControl(worker, controlDone)
	go func() {
		err := command.Wait()
		select {
		case <-controlDone:
		case <-time.After(200 * time.Millisecond):
			_ = control.Close()
			err = errors.Join(err, errors.New("agent worker control did not close after exit"))
			<-controlDone
		}
		b.event(brokerEvent{worker: worker, type_: "waited", err: err})
	}()
	// Hold the lock before returning to the event loop so an immediate cancel
	// cannot overtake initialization on the private control stream.
	worker.controlMu.Lock()
	go func() {
		err := writeWorkerMessage(control, workerMessage{Type: "init", Exec: &spec, Credential: credential})
		worker.controlMu.Unlock()
		if err != nil {
			b.event(brokerEvent{worker: worker, type_: "failed", err: errors.New("initialize agent worker failed")})
		}
	}()
	go b.pumpInput(worker)
	limit := spec.OutputLimitBytes
	if limit == 0 {
		limit = 16 << 20
	}
	go b.pumpOutput(worker, "stdout", output, worker.outAck, &worker.outPending, limit)
	go b.pumpOutput(worker, "stderr", standardError, worker.errAck, &worker.errPending, limit)
	return nil
}

func (b *agentBroker) event(event brokerEvent) {
	select {
	case b.events <- event:
	case <-b.stopped:
	}
}

func (b *agentBroker) watchControl(worker *brokerWorker, done chan<- struct{}) {
	defer close(done)
	reader := bufio.NewReader(worker.control)
	for {
		message, err := readWorkerMessage(reader)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil || message.Type != "exited" {
			b.event(brokerEvent{worker: worker, type_: "failed", err: errors.New("agent worker control protocol failed")})
			return
		}
		b.event(brokerEvent{worker: worker, type_: "exited", message: message})
	}
}

func (b *agentBroker) handleEvent(event brokerEvent) {
	worker := event.worker
	switch event.type_ {
	case "exited":
		if worker.exited {
			b.requestStop(errors.New("agent worker reported duplicate native result"))
			return
		}
		worker.exited = true
		worker.foregroundDone.Store(true)
		close(worker.foregroundExit)
		b.output.push(Message{Type: "exited", ID: worker.id, ExitCode: event.message.ExitCode, Error: event.message.Error})
		_ = worker.stdin.Close()
		worker.deadlineOutput()
	case "waited":
		worker.waited = true
		if worker.forceTimer != nil {
			worker.forceTimer.Stop()
		}
		_ = worker.control.Close()
		_ = worker.stdin.Close()
		message := Message{Type: "retired", ID: worker.id}
		if event.err != nil || !worker.exited {
			message.Error = "agent worker exited without confirmed cleanup"
			b.requestStop(errors.New(message.Error))
			worker.closeOutput()
		}
		b.output.push(message)
	case "output_done":
		worker.outputDone++
	case "failed":
		b.requestStop(event.err)
	}
	if worker.waited && worker.outputDone == 2 {
		delete(b.workers, worker.id)
	}
}

func (worker *brokerWorker) stop() {
	worker.stopOnce.Do(func() {
		close(worker.stopRequested)
		worker.closeStreams()
		go func() {
			worker.controlMu.Lock()
			_ = writeWorkerMessage(worker.control, workerMessage{Type: "stop"})
			worker.controlMu.Unlock()
		}()
		worker.forceTimer = time.AfterFunc(supervisorGrace+time.Second, func() { _ = worker.command.Process.Kill() })
	})
}

func (worker *brokerWorker) closeOutput() {
	_ = worker.stdout.Close()
	_ = worker.stderr.Close()
}

func (worker *brokerWorker) deadlineOutput() {
	deadline := time.Now().Add(200 * time.Millisecond)
	worker.outputDeadline.Store(&deadline)
	_ = worker.stdout.SetReadDeadline(deadline)
	_ = worker.stderr.SetReadDeadline(deadline)
}

func (worker *brokerWorker) closeStreams() {
	_ = worker.stdin.Close()
	worker.closeOutput()
}

func (b *agentBroker) pumpInput(worker *brokerWorker) {
	for {
		select {
		case input := <-worker.input:
			if input.eof {
				_ = worker.stdin.Close()
				return
			}
			err := writeProtocolBytes(worker.stdin, input.data)
			worker.inPending.Store(false)
			if worker.foregroundDone.Load() {
				return
			}
			message := Message{Type: "stdin_ack", ID: worker.id}
			if err != nil {
				message.Error = "write agent stdin failed"
			}
			b.output.push(message)
			if err != nil {
				return
			}
		case <-worker.stopRequested:
			return
		case <-worker.foregroundExit:
			return
		case <-b.stopped:
			return
		}
	}
}

func (b *agentBroker) pumpOutput(worker *brokerWorker, stream string, file *os.File, ack <-chan struct{}, pending *atomic.Bool, limit int) {
	defer b.event(brokerEvent{worker: worker, type_: "output_done"})
	defer b.output.push(Message{Type: stream + "_eof", ID: worker.id})
	defer file.Close()
	buffer := make([]byte, MaxChunkBytes)
	total, remaining := 0, -1
	for {
		// The foreground drain window never slides with background writes.
		// After that fixed window, snapshot one finite pipe tail. Taking this
		// snapshot after a delayed ACK preserves bytes already buffered while
		// the host was busy without following subsequent background output.
		if deadline := worker.outputDeadline.Load(); remaining < 0 && deadline != nil && !time.Now().Before(*deadline) {
			var err error
			remaining, err = outputTailBytes(file)
			if err != nil {
				if !errors.Is(err, os.ErrClosed) {
					b.event(brokerEvent{worker: worker, type_: "failed", err: errors.New("inspect agent output tail failed")})
				}
				return
			}
		}
		if remaining == 0 {
			return
		}
		var count int
		var err error
		if remaining < 0 {
			count, err = file.Read(buffer)
		} else {
			count, err = readOutputTail(file, buffer[:min(len(buffer), remaining)])
			remaining -= count
		}
		if count > 0 {
			total += count
			if total > limit {
				b.event(brokerEvent{worker: worker, type_: "failed", err: errors.New("agent output exceeded its byte limit")})
				return
			}
			pending.Store(true)
			b.output.push(Message{Type: stream, ID: worker.id, Data: bytes.Clone(buffer[:count])})
			select {
			case <-ack:
			case <-worker.stopRequested:
				return
			case <-b.stop:
				return
			}
		}
		if err != nil {
			if remaining < 0 && errors.Is(err, os.ErrDeadlineExceeded) {
				continue // The next iteration takes the single bounded snapshot.
			}
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				b.event(brokerEvent{worker: worker, type_: "failed", err: errors.New("read agent output failed")})
			}
			return
		}
	}
}

func outputTailBytes(file *os.File) (int, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var count int
	var inspectErr error
	err = raw.Control(func(fd uintptr) { count, inspectErr = unix.IoctlGetInt(int(fd), unix.TIOCINQ) })
	return count, errors.Join(err, inspectErr)
}

func readOutputTail(file *os.File, buffer []byte) (int, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var count int
	var readErr error
	// os.Pipe's reader stays nonblocking and is never passed to task code.
	// A task can reopen its own pipe endpoint and consume the snapshotted
	// bytes; one nonblocking read then ends the drain instead of hanging.
	err = raw.Control(func(fd uintptr) {
		for {
			count, readErr = unix.Read(int(fd), buffer)
			if !errors.Is(readErr, unix.EINTR) {
				break
			}
		}
	})
	if err != nil {
		return 0, err
	}
	if errors.Is(readErr, unix.EAGAIN) || count == 0 && readErr == nil {
		return 0, io.EOF
	}
	return count, readErr
}

func (b *agentBroker) liveWorkers() int {
	count := 0
	for _, worker := range b.workers {
		if !worker.waited {
			count++
		}
	}
	return count
}

func (b *agentBroker) pendingOutputs() int {
	count := 0
	for _, worker := range b.workers {
		count += 2 - worker.outputDone
	}
	return count
}

// Output has one writer. Queueing control never waits for a blocked stdout;
// stream pumps are separately bounded by their one-chunk acknowledgement.
type brokerOutput struct {
	mu      sync.Mutex
	queue   []Message
	writing bool
	err     error
	changed chan struct{}
	wake    chan struct{}
}

func newBrokerOutput(writer io.Writer, failed func(error)) *brokerOutput {
	output := &brokerOutput{changed: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
	go func() {
		for range output.wake {
			for {
				output.mu.Lock()
				if len(output.queue) == 0 {
					output.mu.Unlock()
					break
				}
				message := output.queue[0]
				output.queue[0] = Message{}
				output.queue = output.queue[1:]
				output.writing = true
				output.mu.Unlock()
				err := WriteMessage(writer, message)
				output.mu.Lock()
				output.writing = false
				output.err = err
				output.mu.Unlock()
				select {
				case output.changed <- struct{}{}:
				default:
				}
				if err != nil {
					failed(errors.New("write agent broker protocol failed"))
					return
				}
			}
		}
	}()
	return output
}

func (output *brokerOutput) push(message Message) {
	output.mu.Lock()
	if output.err == nil {
		output.queue = append(output.queue, message)
	}
	output.mu.Unlock()
	select {
	case output.wake <- struct{}{}:
	default:
	}
}

func (output *brokerOutput) flush(ctx context.Context) error {
	for {
		output.mu.Lock()
		done, err := len(output.queue) == 0 && !output.writing, output.err
		output.mu.Unlock()
		if err != nil {
			return errors.New("agent broker output failed")
		}
		if done {
			return nil
		}
		select {
		case <-output.changed:
		case <-ctx.Done():
			return fmt.Errorf("drain agent broker output: %w", ctx.Err())
		}
	}
}
