package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer stands in for one native `codex exec-server --listen stdio`. It
// requires the handshake first, answers one-shot methods, and runs at most
// one process, as a fresh native server would for this executor.
type fakeServer struct {
	mu       sync.Mutex
	received []string
	sawEOF   bool
}

func (s *fakeServer) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.received...)
}

func (s *fakeServer) eof() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.sawEOF }

func (s *fakeServer) serve(in io.Reader, out io.Writer) {
	reader := bufio.NewReader(in)
	write := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	initialized, initializeSeen := false, false
	process := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			s.mu.Lock()
			s.sawEOF = true
			s.mu.Unlock()
			return
		}
		line = strings.TrimSpace(line)
		s.mu.Lock()
		s.received = append(s.received, line)
		s.mu.Unlock()
		var m message
		_ = json.Unmarshal([]byte(line), &m)
		switch {
		case m.Method == "initialize" && initializeSeen:
			// The real server accepts initialize once per connection.
			write(`{"id":%s,"error":{"code":-32600,"message":"initialize may only be sent once per connection"}}`, m.ID)
		case m.Method == "initialize":
			initializeSeen = true
			write(`{"id":%s,"result":{"sessionId":"native-session"}}`, m.ID)
		case m.Method == "initialized":
			initialized = true
		case !initialized && m.request():
			write(`{"id":%s,"error":{"code":-32600,"message":"not initialized"}}`, m.ID)
		case m.Method == "fs/getMetadata":
			write(`{"id":%s,"result":{"isDirectory":true}}`, m.ID)
		case m.Method == "ask/client":
			write(`{"id":7,"method":"approval/request","params":{}}`)
		case m.Method == "process/start":
			id, _ := processID(m.Params)
			if process != "" || strings.Contains(line, "fail-start") {
				write(`{"id":%s,"error":{"code":-32600,"message":"cannot start"}}`, m.ID)
				continue
			}
			process = id
			write(`{"id":%s,"result":{"processId":%q}}`, m.ID, id)
			if strings.Contains(line, "exit-now") {
				write(`{"method":"process/exited","params":{"processId":%q,"seq":1,"exitCode":0}}`, id)
				write(`{"method":"process/closed","params":{"processId":%q,"seq":2}}`, id)
			}
		case m.Method == "process/write":
			id, _ := processID(m.Params)
			write(`{"id":%s,"result":{"status":"accepted"}}`, m.ID)
			write(`{"method":"process/output","params":{"processId":%q,"seq":1,"stream":"stdout","chunk":"b2s="}}`, id)
		case m.Method == "process/terminate":
			id, _ := processID(m.Params)
			write(`{"id":%s,"result":{"running":true}}`, m.ID)
			write(`{"method":"process/closed","params":{"processId":%q,"seq":3}}`, id)
		case m.response():
			write(`{"method":"approval/answered","params":{"echo":%s}}`, line)
		}
	}
}

type fakeChannel struct {
	stdin  *io.PipeWriter
	stdout *io.PipeReader
	done   chan struct{}
}

func (c *fakeChannel) Stdin() io.WriteCloser { return c.stdin }
func (c *fakeChannel) Stdout() io.Reader     { return c.stdout }
func (c *fakeChannel) Wait() error           { <-c.done; return nil }
func (c *fakeChannel) Close() error          { _ = c.stdin.Close(); return nil }

type fakeOpener struct {
	mu      sync.Mutex
	servers []*fakeServer
	fail    error
}

func (o *fakeOpener) Open(_ context.Context, remote string) (Channel, error) {
	if remote != RemoteCommand {
		return nil, fmt.Errorf("remote %q", remote)
	}
	if o.fail != nil {
		return nil, o.fail
	}
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	server := &fakeServer{}
	o.mu.Lock()
	o.servers = append(o.servers, server)
	o.mu.Unlock()
	channel := &fakeChannel{stdin: inWriter, stdout: outReader, done: make(chan struct{})}
	go func() {
		defer close(channel.done)
		server.serve(inReader, outWriter)
		_ = outWriter.Close()
	}()
	return channel, nil
}

func (o *fakeOpener) all() []*fakeServer {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*fakeServer(nil), o.servers...)
}

// harness drives a Router like Codex does: write lines, read replies by ID.
type harness struct {
	t       *testing.T
	in      *io.PipeWriter
	replies chan message
	served  chan error
}

func startRouter(t *testing.T, opener Opener) *harness {
	t.Helper()
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	h := &harness{t: t, in: inWriter, replies: make(chan message, 64), served: make(chan error, 1)}
	router := NewRouter(context.Background(), opener, outWriter, io.Discard)
	go func() { h.served <- router.Serve(inReader); _ = outWriter.Close() }()
	go func() {
		scanner := bufio.NewScanner(outReader)
		for scanner.Scan() {
			var m message
			if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
				t.Errorf("router wrote invalid JSON %q", scanner.Text())
				continue
			}
			h.replies <- m
		}
		close(h.replies)
	}()
	return h
}

func (h *harness) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.in, line+"\n"); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) next() message {
	h.t.Helper()
	select {
	case m, ok := <-h.replies:
		if !ok {
			h.t.Fatal("router output closed")
		}
		return m
	case <-time.After(5 * time.Second):
		h.t.Fatal("timed out waiting for router output")
	}
	return message{}
}

func (h *harness) stop() {
	h.t.Helper()
	_ = h.in.Close()
	select {
	case err := <-h.served:
		if err != nil {
			h.t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("router did not finish after EOF")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEveryOneShotCallGetsItsOwnHandshakenChannel(t *testing.T) {
	opener := &fakeOpener{}
	h := startRouter(t, opener)
	h.send(`{"id":1,"method":"initialize","params":{"clientName":"codex-environment","resumeSessionId":null}}`)
	if m := h.next(); string(m.ID) != "1" || !strings.Contains(string(m.Result), "native-session") {
		t.Fatalf("initialize reply = %+v", m)
	}
	h.send(`{"method":"initialized","params":{}}`)
	h.send(`{"id":2,"method":"fs/getMetadata","params":{"path":"file:///app/.git","sandbox":null}}`)
	if m := h.next(); string(m.ID) != "2" || !strings.Contains(string(m.Result), "isDirectory") {
		t.Fatalf("metadata reply = %+v", m)
	}
	h.send(`{"id":3,"method":"fs/getMetadata","params":{"path":"file:///app","sandbox":null}}`)
	if m := h.next(); string(m.ID) != "3" {
		t.Fatalf("metadata reply = %+v", m)
	}
	servers := opener.all()
	if len(servers) != 3 {
		t.Fatalf("channels = %d, want one per call", len(servers))
	}
	for index, server := range servers {
		waitFor(t, server.eof)
		lines := server.lines()
		// Codex's initialize is answered by its channel's own handshake.
		want := 3
		if index == 0 {
			want = 2
		}
		if len(lines) != want || !strings.Contains(lines[0], `"id":"aries-initialize"`) || !strings.Contains(lines[0], "codex-environment") || !strings.Contains(lines[1], `"initialized"`) {
			t.Fatalf("channel %d did not start with exactly one handshake: %q", index, lines)
		}
	}
	if !strings.Contains(servers[1].lines()[2], "file:///app/.git") {
		t.Fatalf("request not forwarded unchanged: %q", servers[1].lines())
	}
	h.stop()
}

func TestProcessRequestsShareTheProcessChannelUntilClosed(t *testing.T) {
	opener := &fakeOpener{}
	h := startRouter(t, opener)
	h.send(`{"id":10,"method":"process/start","params":{"processId":"p1","argv":["/bin/bash","-c","cat"],"tty":true}}`)
	if m := h.next(); string(m.ID) != "10" || len(m.Error) != 0 {
		t.Fatalf("start reply = %+v", m)
	}
	h.send(`{"id":11,"method":"process/write","params":{"processId":"p1","chunk":"aGk=","writeId":"w1"}}`)
	for _, want := range []string{"11", "process/output"} {
		m := h.next()
		if string(m.ID) != want && m.Method != want {
			t.Fatalf("got %+v, want %s", m, want)
		}
	}
	h.send(`{"id":12,"method":"process/terminate","params":{"processId":"p1"}}`)
	terminate, closed := h.next(), h.next()
	if string(terminate.ID) != "12" || !strings.Contains(string(terminate.Result), `"running":true`) || closed.Method != "process/closed" {
		t.Fatalf("terminate = %+v, closed = %+v", terminate, closed)
	}
	servers := opener.all()
	if len(servers) != 1 {
		t.Fatalf("process used %d channels, want 1", len(servers))
	}
	waitFor(t, servers[0].eof)
	// The native server has gone; a late terminate gets the exited answer.
	h.send(`{"id":13,"method":"process/terminate","params":{"processId":"p1"}}`)
	if m := h.next(); string(m.ID) != "13" || string(m.Result) != `{"running":false}` {
		t.Fatalf("late terminate = %+v", m)
	}
	h.send(`{"id":14,"method":"process/write","params":{"processId":"p1","chunk":"","writeId":"w2"}}`)
	if m := h.next(); string(m.ID) != "14" || !strings.Contains(string(m.Result), "unknownProcess") {
		t.Fatalf("late write = %+v", m)
	}
	h.stop()
}

func TestConcurrentProcessesUseSeparateChannels(t *testing.T) {
	opener := &fakeOpener{}
	h := startRouter(t, opener)
	h.send(`{"id":1,"method":"process/start","params":{"processId":"a","argv":["sleep","9"]}}`)
	h.send(`{"id":2,"method":"process/start","params":{"processId":"b","argv":["sleep","9"]}}`)
	seen := map[string]bool{string(h.next().ID): true, string(h.next().ID): true}
	if !seen["1"] || !seen["2"] || len(opener.all()) != 2 {
		t.Fatalf("replies %v, channels %d", seen, len(opener.all()))
	}
	h.send(`{"id":3,"method":"process/start","params":{"processId":"a","argv":["true"]}}`)
	if m := h.next(); string(m.ID) != "3" || !strings.Contains(string(m.Error), "already in use") {
		t.Fatalf("duplicate start = %+v", m)
	}
	// EOF from Codex ends each live server, which terminates what it started.
	h.stop()
	for _, server := range opener.all() {
		if !server.eof() {
			t.Fatal("live process channel left open after Codex exited")
		}
	}
}

func TestFailedStartReleasesItsChannelAndID(t *testing.T) {
	opener := &fakeOpener{}
	h := startRouter(t, opener)
	h.send(`{"id":1,"method":"process/start","params":{"processId":"p","argv":["fail-start"]}}`)
	if m := h.next(); string(m.ID) != "1" || len(m.Error) == 0 {
		t.Fatalf("failed start = %+v", m)
	}
	waitFor(t, opener.all()[0].eof)
	h.send(`{"id":2,"method":"process/start","params":{"processId":"p","argv":["exit-now"]}}`)
	reply, exited, closed := h.next(), h.next(), h.next()
	if string(reply.ID) != "2" || len(reply.Error) != 0 || exited.Method != "process/exited" || closed.Method != "process/closed" {
		t.Fatalf("restart = %+v %+v %+v", reply, exited, closed)
	}
	waitFor(t, opener.all()[1].eof)
	h.stop()
}

func TestServerRequestsAreRenumberedAndAnsweredOnTheirChannel(t *testing.T) {
	opener := &fakeOpener{}
	h := startRouter(t, opener)
	h.send(`{"id":1,"method":"ask/client","params":{}}`)
	request := h.next()
	if request.Method != "approval/request" || string(request.ID) == "7" {
		t.Fatalf("server request = %+v", request)
	}
	h.send(fmt.Sprintf(`{"id":%s,"result":{"approved":true}}`, request.ID))
	answered := h.next()
	if answered.Method != "approval/answered" || !strings.Contains(string(answered.Params), `"id":7`) {
		t.Fatalf("server did not receive its own id back: %+v", answered)
	}
	_ = h.in.Close()
}

func TestOpenFailureAnswersTheCall(t *testing.T) {
	h := startRouter(t, &fakeOpener{fail: errors.New("bridge unreachable")})
	h.send(`{"id":5,"method":"fs/readFile","params":{"path":"file:///app/x"}}`)
	if m := h.next(); string(m.ID) != "5" || !strings.Contains(string(m.Error), "bridge unreachable") {
		t.Fatalf("reply = %+v", m)
	}
	h.send(`{"id":6,"method":"process/start","params":{"processId":"p","argv":["true"]}}`)
	if m := h.next(); string(m.ID) != "6" || !strings.Contains(string(m.Error), "bridge unreachable") {
		t.Fatalf("reply = %+v", m)
	}
	h.send(`{"id":7,"method":"process/terminate","params":{"processId":"p"}}`)
	if m := h.next(); string(m.ID) != "7" || string(m.Result) != `{"running":false}` {
		t.Fatalf("terminate after failed open = %+v", m)
	}
	h.stop()
}

func TestMainRejectsAnyOtherInvocation(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--address"},
		{"--address", "127.0.0.1:2222", "extra"},
		{"--host", "127.0.0.1:2222"},
		{"--address", "127.0.0.1"},
		{"--address", ":2222"},
		{"--address", "127.0.0.1:0"},
		{"--address", "127.0.0.1:70000"},
		{"--address", "bad host:22"},
	} {
		var stderr strings.Builder
		if code := Main(context.Background(), args, io.NopCloser(strings.NewReader("")), io.Discard, &stderr); code != 255 || !strings.Contains(stderr.String(), "aries-codex-ssh:") {
			t.Fatalf("args %q: code=%d stderr=%q", args, code, stderr.String())
		}
	}
}

type lockedBuffer struct {
	mu   sync.Mutex
	data strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}

func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.data.String() }

// A request routed through a channel pointer taken before the channel was
// forgotten must still be answered, never queued where nothing reads it.
func TestStaleProcessChannelAnswersInsteadOfQueueing(t *testing.T) {
	for name, state := range map[string]func(*callChannel){
		"process closed":   func(c *callChannel) { c.closed = true },
		"channel finished": func(c *callChannel) { c.finished = true },
		"channel failed":   func(c *callChannel) { c.failed = errors.New("stream lost") },
	} {
		t.Run(name, func(t *testing.T) {
			out := &lockedBuffer{}
			router := NewRouter(context.Background(), &fakeOpener{}, out, io.Discard)
			channel := &callChannel{router: router, process: "p", pending: map[string]bool{}, wake: make(chan struct{}, 1)}
			state(channel)
			channel.send(json.RawMessage("7"), []byte(`{"id":7,"method":"process/terminate","params":{"processId":"p"}}`))
			channel.send(json.RawMessage("8"), []byte(`{"id":8,"method":"process/write","params":{"processId":"p","chunk":"","writeId":"w"}}`))
			if len(channel.queue) != 0 || len(channel.pending) != 0 {
				t.Fatalf("request queued on a gone channel: queue=%d pending=%d", len(channel.queue), len(channel.pending))
			}
			got := out.String()
			if name == "channel failed" {
				if !strings.Contains(got, `"id":7`) || !strings.Contains(got, "stream lost") || !strings.Contains(got, `"id":8`) {
					t.Fatalf("failed channel replies = %s", got)
				}
				return
			}
			if !strings.Contains(got, `{"id":7,"result":{"running":false}}`) || !strings.Contains(got, `{"id":8,"result":{"status":"unknownProcess"}}`) {
				t.Fatalf("replies = %s", got)
			}
		})
	}
}

// Codex's initialize must be registered on its channel before the channel's
// worker starts: a worker that opens and reads its handshake reply, or fails
// to open, before the caller's next statement would leave it unanswered. The
// race is rare, so many routers run at once (bounded for the race detector's
// goroutine limit).
func TestInitializeIsAnsweredHoweverFastItsChannelRuns(t *testing.T) {
	for name, opener := range map[string]func() Opener{
		"open fails":     func() Opener { return &fakeOpener{fail: errors.New("bridge unreachable")} },
		"server answers": func() Opener { return &fakeOpener{} },
	} {
		t.Run(name, func(t *testing.T) {
			var lost atomic.Int32
			var wg sync.WaitGroup
			live := make(chan struct{}, 500)
			for range 2000 {
				wg.Add(1)
				live <- struct{}{}
				go func() {
					defer func() { <-live; wg.Done() }()
					out := &lockedBuffer{}
					router := NewRouter(context.Background(), opener(), out, io.Discard)
					router.dispatch([]byte(`{"id":1,"method":"initialize","params":{"clientName":"codex-environment"}}`))
					done := make(chan struct{})
					go func() { router.channels.Wait(); close(done) }()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						lost.Add(1) // A channel left waiting for an answer.
						return
					}
					if !strings.Contains(out.String(), `"id":1,`) {
						lost.Add(1)
					}
				}()
			}
			wg.Wait()
			if lost.Load() != 0 {
				t.Fatalf("%d of 2000 initialize requests were never answered", lost.Load())
			}
		})
	}
}
