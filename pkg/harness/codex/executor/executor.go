// Package executor is the stdio program Codex's native remote environment
// spawns inside the harness container. Codex 0.157.1 speaks exec-server
// JSON-RPC to it; the executor keeps that one logical session toward Codex and
// gives every call its own SSH channel to a fresh native `codex exec-server`
// in the task sandbox, so the bridge sees and records one exec per call, as it
// does for every other harness.
//
// One-shot requests (initialize, fs/*, environment queries) use a channel
// that closes once the response arrives. A process keeps its channel until
// the native server reports process/closed, and every later request naming
// that process (write, signal, terminate) goes to the same server, so tty,
// signal, termination, and environment handling stay native. The executor
// reads only routing fields and forwards each message unchanged.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

// RemoteCommand is the only exec request the bridge's Codex dialect accepts.
// It names the native executor rather than a shell command.
const RemoteCommand = "aries-codex-exec-server-v1"

const (
	handshakeID       = `"aries-initialize"`
	initializeMethod  = "initialize"
	initializedMethod = "initialized"
	processStart      = "process/start"
	processClosed     = "process/closed"
	processTerminate  = "process/terminate"
	processWrite      = "process/write"
)

// Opener starts one remote command and returns its streams. Production uses
// one SSH connection with a session per call; tests substitute pipes.
type Opener interface {
	Open(ctx context.Context, remote string) (Channel, error)
}

// Channel is one remote exec: its input, output, and completion.
type Channel interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Wait() error
	Close() error
}

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func (m message) request() bool      { return len(m.ID) != 0 && m.Method != "" }
func (m message) notification() bool { return len(m.ID) == 0 && m.Method != "" }
func (m message) response() bool     { return len(m.ID) != 0 && m.Method == "" }

// Router multiplexes Codex's session onto per-call channels.
type Router struct {
	opener Opener
	ctx    context.Context
	log    io.Writer

	writeMu sync.Mutex
	out     io.Writer

	mu         sync.Mutex
	initialize json.RawMessage
	processes  map[string]*callChannel
	serverIDs  map[string]serverRequest
	nextServer uint64
	channels   sync.WaitGroup
}

type serverRequest struct {
	channel *callChannel
	id      json.RawMessage
}

// NewRouter writes Codex-bound messages to out and diagnostics to log.
func NewRouter(ctx context.Context, opener Opener, out, log io.Writer) *Router {
	return &Router{opener: opener, ctx: ctx, out: out, log: log, processes: map[string]*callChannel{}, serverIDs: map[string]serverRequest{}}
}

// Serve reads Codex's messages until EOF, then closes every channel's input so
// each native server shuts down, terminating what it started, and waits.
func (r *Router) Serve(in io.Reader) error {
	reader := bufio.NewReader(in)
	var readErr error
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) != 0 {
			r.dispatch(bytes.TrimRight(line, "\r\n"))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	r.mu.Lock()
	open := make([]*callChannel, 0, len(r.processes))
	for _, channel := range r.processes {
		open = append(open, channel)
	}
	r.mu.Unlock()
	for _, channel := range open {
		channel.finish()
	}
	r.channels.Wait()
	return readErr
}

func (r *Router) dispatch(line []byte) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		r.logf("drop malformed message from Codex: %v", err)
		return
	}
	switch {
	case m.request():
		r.routeRequest(m, line)
	case m.notification():
		r.routeNotification(m, line)
	case m.response():
		r.routeServerResponse(m, line)
	default:
		r.logf("drop message without method or id from Codex")
	}
}

func (r *Router) routeRequest(m message, line []byte) {
	switch m.Method {
	case initializeMethod:
		// A server accepts initialize once per connection, so Codex's own
		// initialize is the handshake of its channel and gets that answer.
		r.mu.Lock()
		r.initialize = append(json.RawMessage(nil), m.Params...)
		channel := r.newChannel("")
		r.mu.Unlock()
		channel.answerHandshake(m.ID)
	case processStart:
		id, ok := processID(m.Params)
		if !ok {
			r.replyError(m.ID, -32602, "process/start requires processId")
			return
		}
		r.mu.Lock()
		if _, exists := r.processes[id]; exists {
			r.mu.Unlock()
			r.replyError(m.ID, -32600, "process id already in use: "+id)
			return
		}
		channel := r.newChannel(id)
		channel.startID = string(m.ID)
		r.processes[id] = channel
		r.mu.Unlock()
		channel.send(m.ID, line)
	default:
		if id, ok := processID(m.Params); ok && isProcessMethod(m.Method) {
			r.mu.Lock()
			channel := r.processes[id]
			r.mu.Unlock()
			if channel == nil {
				r.replyClosedProcess(m)
				return
			}
			channel.send(m.ID, line)
			return
		}
		r.oneShot(m, line)
	}
}

// A process whose server already reported process/closed is gone; answer
// with what that server would have answered for an exited process.
func (r *Router) replyClosedProcess(m message) {
	switch m.Method {
	case processTerminate:
		r.reply(m.ID, json.RawMessage(`{"running":false}`))
	case processWrite:
		r.reply(m.ID, json.RawMessage(`{"status":"unknownProcess"}`))
	default:
		r.replyError(m.ID, -32600, "unknown process")
	}
}

func (r *Router) oneShot(m message, line []byte) {
	r.mu.Lock()
	channel := r.newChannel("")
	r.mu.Unlock()
	channel.send(m.ID, line)
}

func (r *Router) routeNotification(m message, line []byte) {
	if m.Method == initializedMethod {
		return // Every channel completes its own handshake.
	}
	if id, ok := processID(m.Params); ok {
		r.mu.Lock()
		channel := r.processes[id]
		r.mu.Unlock()
		if channel != nil {
			channel.send(nil, line)
			return
		}
	}
	r.logf("drop notification %q with no live process", m.Method)
}

func (r *Router) routeServerResponse(m message, line []byte) {
	r.mu.Lock()
	request, ok := r.serverIDs[string(m.ID)]
	delete(r.serverIDs, string(m.ID))
	r.mu.Unlock()
	if !ok {
		r.logf("drop response to unknown server request %s", m.ID)
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil {
		return
	}
	fields["id"] = request.id
	restored, err := json.Marshal(fields)
	if err != nil {
		return
	}
	request.channel.send(nil, restored)
}

// Server-originated requests get router-unique IDs, since every channel's
// server numbers its own requests.
func (r *Router) forwardServerRequest(channel *callChannel, line []byte) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(line, &fields) != nil {
		return
	}
	r.mu.Lock()
	r.nextServer++
	id := json.RawMessage(strconv.Quote("aries-server-" + strconv.FormatUint(r.nextServer, 10)))
	r.serverIDs[string(id)] = serverRequest{channel: channel, id: fields["id"]}
	r.mu.Unlock()
	fields["id"] = id
	if rewritten, err := json.Marshal(fields); err == nil {
		r.write(rewritten)
	}
}

func (r *Router) reply(id, result json.RawMessage) {
	encoded, _ := json.Marshal(struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}{id, result})
	r.write(encoded)
}

func (r *Router) replyError(id json.RawMessage, code int, text string) {
	encoded, _ := json.Marshal(struct {
		ID    json.RawMessage `json:"id"`
		Error any             `json:"error"`
	}{id, map[string]any{"code": code, "message": text}})
	r.write(encoded)
}

func (r *Router) write(line []byte) {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if _, err := r.out.Write(append(append([]byte(nil), line...), '\n')); err != nil {
		r.logf("write to Codex: %v", err)
	}
}

func (r *Router) logf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.log, "aries-codex-ssh: "+format+"\n", args...)
}

func processID(params json.RawMessage) (string, bool) {
	var fields struct {
		ProcessID json.RawMessage `json:"processId"`
	}
	if len(params) == 0 || json.Unmarshal(params, &fields) != nil || len(fields.ProcessID) == 0 {
		return "", false
	}
	var text string
	if json.Unmarshal(fields.ProcessID, &text) == nil {
		return text, true
	}
	return string(fields.ProcessID), true
}

func isProcessMethod(method string) bool {
	return len(method) > len("process/") && method[:len("process/")] == "process/"
}

// callChannel serves one call. Sends are queued in order and written once the
// channel is open and its handshake is on the wire.
type callChannel struct {
	router           *Router
	process, startID string
	handshakeFor     json.RawMessage // Codex's initialize ID, when this channel answers it

	mu       sync.Mutex
	queue    [][]byte
	pending  map[string]bool
	closed   bool // process/closed seen, or the one-shot answered
	finished bool
	failed   error
	wake     chan struct{}
}

func (r *Router) newChannel(process string) *callChannel {
	channel := &callChannel{router: r, process: process, pending: map[string]bool{}, wake: make(chan struct{}, 1)}
	r.channels.Add(1)
	go channel.run()
	return channel
}

// A caller may hold this channel from before it was forgotten, so a send can
// arrive after the process closed or the channel ended; it is answered here
// rather than queued where nothing would read it.
func (c *callChannel) send(id json.RawMessage, line []byte) {
	c.mu.Lock()
	if c.failed != nil || c.finished || c.process != "" && c.closed {
		failed := c.failed
		c.mu.Unlock()
		if len(id) == 0 {
			return
		}
		if failed == nil && c.process != "" {
			var m message
			_ = json.Unmarshal(line, &m)
			m.ID = id
			c.router.replyClosedProcess(m)
			return
		}
		reason := "aries executor channel already closed"
		if failed != nil {
			reason = "aries executor channel failed: " + failed.Error()
		}
		c.router.replyError(id, -32603, reason)
		return
	}
	if len(id) != 0 {
		c.pending[string(id)] = true
	}
	c.queue = append(c.queue, append([]byte(nil), line...))
	c.mu.Unlock()
	c.signal()
}

// answerHandshake makes this channel's handshake response the answer to
// Codex's initialize request id.
func (c *callChannel) answerHandshake(id json.RawMessage) {
	c.mu.Lock()
	c.handshakeFor = append(json.RawMessage(nil), id...)
	c.pending[string(id)] = true
	c.mu.Unlock()
	c.signal()
}

// finish closes input once nothing else will be sent; the native server then
// shuts down and the channel ends.
func (c *callChannel) finish() {
	c.mu.Lock()
	c.finished = true
	c.mu.Unlock()
	c.signal()
}

func (c *callChannel) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *callChannel) run() {
	defer c.router.channels.Done()
	channel, err := c.router.opener.Open(c.router.ctx, RemoteCommand)
	if err != nil {
		// Forget first, so a request that follows the failure reply sees no
		// live process.
		c.forget()
		c.fail(err)
		return
	}
	defer channel.Close()
	c.router.mu.Lock()
	initialize := c.router.initialize
	c.router.mu.Unlock()
	if len(initialize) == 0 {
		initialize = json.RawMessage(`{"clientName":"codex-environment","resumeSessionId":null}`)
	}
	handshake := fmt.Sprintf("{\"id\":%s,\"method\":%q,\"params\":%s}\n{\"method\":%q,\"params\":{}}\n", handshakeID, initializeMethod, initialize, initializedMethod)
	stdin := channel.Stdin()
	writeErr := make(chan error, 1)
	if _, err := io.WriteString(stdin, handshake); err != nil {
		writeErr <- err
	}
	readDone := make(chan error, 1)
	go func() { readDone <- c.read(channel.Stdout()) }()
	inputClosed := false
	for {
		c.mu.Lock()
		queue := c.queue
		c.queue = nil
		done := c.finished || c.closed && len(c.pending) == 0
		c.mu.Unlock()
		for _, line := range queue {
			if _, err := stdin.Write(append(line, '\n')); err != nil {
				select {
				case writeErr <- err:
				default:
				}
				break
			}
		}
		if done && !inputClosed {
			_ = stdin.Close()
			inputClosed = true
		}
		select {
		case <-c.wake:
			continue
		case err := <-writeErr:
			c.fail(fmt.Errorf("write to native executor: %w", err))
			_ = channel.Close()
		case err := <-readDone:
			waitErr := channel.Wait()
			c.forget()
			c.mu.Lock()
			c.finished = true // Later sends are answered by send, not queued.
			unanswered := len(c.pending) != 0
			c.mu.Unlock()
			if unanswered {
				c.fail(errors.Join(err, waitErr, errors.New("native executor ended before answering")))
			} else if err != nil {
				c.router.logf("native executor stream: %v", err)
			}
			return
		}
		// After a write failure, wait for the reader to end.
		err := <-readDone
		_ = channel.Wait()
		c.forget()
		c.fail(err)
		return
	}
}

// read forwards the native server's messages to Codex until its stdout ends.
func (c *callChannel) read(stdout io.Reader) error {
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimRight(line, "\r\n"); len(bytes.TrimSpace(trimmed)) != 0 {
			c.forward(trimmed)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (c *callChannel) forward(line []byte) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		c.router.logf("drop malformed message from native executor: %v", err)
		return
	}
	switch {
	case m.response() && string(m.ID) == handshakeID:
		c.mu.Lock()
		answer := c.handshakeFor
		if answer != nil {
			delete(c.pending, string(answer))
			c.closed = true
		}
		c.mu.Unlock()
		if answer == nil {
			if len(m.Error) != 0 {
				c.router.logf("native executor rejected initialize: %s", m.Error)
			}
			return
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(line, &fields) == nil {
			fields["id"] = answer
			if rewritten, err := json.Marshal(fields); err == nil {
				c.router.write(rewritten)
			}
		}
		c.signal()
	case m.response():
		// A one-shot call is done once answered; a process that failed to
		// start will never report process/closed. Forget a finished process
		// before Codex can see the reply, so its next request is answered as
		// for an exited process instead of reaching a closing channel.
		startFailed := c.process != "" && string(m.ID) == c.startID && len(m.Error) != 0
		c.mu.Lock()
		delete(c.pending, string(m.ID))
		if c.process == "" || startFailed {
			c.closed = true
		}
		c.mu.Unlock()
		if startFailed {
			c.forget()
		}
		c.router.write(line)
		c.signal()
	case m.request():
		c.router.forwardServerRequest(c, line)
	case m.notification():
		if m.Method == processClosed && c.process != "" {
			c.mu.Lock()
			c.closed = true
			c.mu.Unlock()
			c.forget()
			c.signal()
		}
		c.router.write(line)
	}
}

// fail answers every request still waiting on this channel.
func (c *callChannel) fail(err error) {
	if err == nil {
		err = errors.New("native executor ended")
	}
	c.mu.Lock()
	if c.failed == nil {
		c.failed = err
	}
	pending := make([]string, 0, len(c.pending))
	for id := range c.pending {
		pending = append(pending, id)
	}
	c.pending = map[string]bool{}
	c.mu.Unlock()
	for _, id := range pending {
		c.router.replyError(json.RawMessage(id), -32603, "aries executor channel failed: "+err.Error())
	}
	if len(pending) != 0 {
		c.router.logf("executor channel failed: %v", err)
	}
}

// forget removes a finished process so later requests get the exited answer.
func (c *callChannel) forget() {
	if c.process == "" {
		return
	}
	c.router.mu.Lock()
	if c.router.processes[c.process] == c {
		delete(c.router.processes, c.process)
	}
	c.router.mu.Unlock()
}

// SSHOpener opens one session per call over a shared connection, redialing
// once if the connection has gone away.
type SSHOpener struct {
	Dial func(context.Context) (*gossh.Client, func() error, error)
	// Stderr receives each native server's diagnostics, which Codex logs.
	Stderr io.Writer

	mu     sync.Mutex
	client *gossh.Client
	close  func() error
}

func (o *SSHOpener) Open(ctx context.Context, remote string) (Channel, error) {
	for attempt := 0; attempt < 2; attempt++ {
		client, err := o.connection(ctx)
		if err != nil {
			return nil, err
		}
		session, err := client.NewSession()
		if err != nil {
			o.drop(client)
			continue
		}
		channel, err := startSession(session, remote, o.Stderr)
		if err != nil {
			return nil, err
		}
		return channel, nil
	}
	return nil, errors.New("open SSH session: connection unavailable")
}

func (o *SSHOpener) connection(ctx context.Context) (*gossh.Client, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.client != nil {
		return o.client, nil
	}
	client, closer, err := o.Dial(ctx)
	if err != nil {
		return nil, err
	}
	o.client, o.close = client, closer
	return client, nil
}

func (o *SSHOpener) drop(client *gossh.Client) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.client == client {
		_ = o.close()
		o.client, o.close = nil, nil
	}
}

// Close closes the shared connection.
func (o *SSHOpener) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.close == nil {
		return nil
	}
	err := o.close()
	o.client, o.close = nil, nil
	return err
}

type sshChannel struct {
	session *gossh.Session
	stdin   io.WriteCloser
	stdout  io.Reader
}

func startSession(session *gossh.Session, remote string, stderr io.Writer) (Channel, error) {
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	if stderr == nil {
		stderr = io.Discard
	}
	session.Stderr = stderr
	if err := session.Start(remote); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("start native executor: %w", err)
	}
	return &sshChannel{session: session, stdin: stdin, stdout: stdout}, nil
}

func (c *sshChannel) Stdin() io.WriteCloser { return c.stdin }
func (c *sshChannel) Stdout() io.Reader     { return c.stdout }
func (c *sshChannel) Wait() error           { return c.session.Wait() }
func (c *sshChannel) Close() error          { return c.session.Close() }
