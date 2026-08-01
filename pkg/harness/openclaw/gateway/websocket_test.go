package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWebSocketDialerConnectsAndCarriesGatewayFrames(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, reader := acceptTestWebSocket(t, writer, request)
		defer connection.Close()
		writeServerFrame(t, connection, Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "nonce-1"}})
		connect := readServerFrame(t, reader)
		if connect["method"] != "connect" {
			t.Errorf("connect method = %v", connect["method"])
		}
		writeServerFrame(t, connection, Frame{"type": "res", "id": connect["id"], "ok": true, "payload": map[string]any{"auth": map[string]any{"scopes": []any{"operator.write"}}}})

		call := readServerFrame(t, reader)
		if call["method"] != "talk.session.create" {
			t.Errorf("call method = %v", call["method"])
		}
		writeServerFrame(t, connection, Frame{"type": "event", "event": "talk.event", "payload": map[string]any{"talkEvent": map[string]any{"type": "session.ready", "sessionId": "s-1", "payload": map[string]any{}}}})
		writeServerFrame(t, connection, Frame{"type": "res", "id": call["id"], "ok": true, "payload": map[string]any{"sessionId": "s-1", "audio": map[string]any{"inputEncoding": "pcm16", "inputSampleRateHz": 24000}}})
	})}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			t.Errorf("serve websocket test server: %v", err)
		}
	}()
	defer server.Shutdown(context.Background())

	dialer, err := NewWebSocketDialer(WebSocketOptions{URL: "ws://" + listener.Addr().String()})
	if err != nil {
		t.Fatalf("NewWebSocketDialer returned error: %v", err)
	}
	client, err := New(dialer, Options{Token: "gateway-token"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	defer client.Close()

	ctx, cancel := contextWithTestTimeout()
	defer cancel()
	summary, err := client.Connect(ctx, ConnectOptions{})
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if scopes := summary.Scopes; len(scopes) != 1 || scopes[0] != "operator.write" {
		t.Fatalf("scopes = %#v", scopes)
	}
	response, err := client.Call(ctx, "talk.session.create", map[string]any{"mode": "realtime"})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if response.Map("payload")["sessionId"] != "s-1" {
		t.Fatalf("response = %#v", response)
	}
	event, err := client.RecvEvent(ctx)
	if err != nil {
		t.Fatalf("RecvEvent returned error: %v", err)
	}
	if event.String("event") != "talk.event" {
		t.Fatalf("event = %#v", event)
	}
}

func contextWithTestTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func acceptTestWebSocket(t *testing.T, writer http.ResponseWriter, request *http.Request) (net.Conn, *bufio.Reader) {
	t.Helper()
	key := request.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		t.Fatal("missing Sec-WebSocket-Key")
	}
	if !headerToken(request.Header, "Connection", "upgrade") || !headerToken(request.Header, "Upgrade", "websocket") {
		t.Fatalf("missing upgrade headers: %#v", request.Header)
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		t.Fatal("response writer does not support hijacking")
	}
	connection, readWriter, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("hijack websocket: %v", err)
	}
	response := bytes.NewBuffer(nil)
	fmt.Fprintf(response, "HTTP/1.1 101 Switching Protocols\r\n")
	fmt.Fprintf(response, "Upgrade: websocket\r\n")
	fmt.Fprintf(response, "Connection: Upgrade\r\n")
	fmt.Fprintf(response, "Sec-WebSocket-Accept: %s\r\n", websocketAccept(key))
	fmt.Fprintf(response, "\r\n")
	if _, err := connection.Write(response.Bytes()); err != nil {
		connection.Close()
		t.Fatalf("write websocket handshake: %v", err)
	}
	return connection, readWriter.Reader
}

func writeServerFrame(t *testing.T, connection net.Conn, frame Frame) {
	t.Helper()
	content, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal server frame: %v", err)
	}
	if _, err := connection.Write(encodeServerTextFrame(content)); err != nil {
		t.Fatalf("write server frame: %v", err)
	}
}

func readServerFrame(t *testing.T, reader *bufio.Reader) Frame {
	t.Helper()
	_, content, err := readWebSocketFrame(reader)
	if err != nil {
		t.Fatalf("read server frame: %v", err)
	}
	var frame Frame
	if err := json.Unmarshal(content, &frame); err != nil {
		t.Fatalf("decode server frame: %v", err)
	}
	return frame
}

func encodeServerTextFrame(payload []byte) []byte {
	frame := bytes.NewBuffer(nil)
	frame.WriteByte(0x81)
	switch length := len(payload); {
	case length < 126:
		frame.WriteByte(byte(length))
	case length <= 0xffff:
		frame.WriteByte(126)
		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], uint16(length))
		frame.Write(encoded[:])
	default:
		frame.WriteByte(127)
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(length))
		frame.Write(encoded[:])
	}
	frame.Write(payload)
	return frame.Bytes()
}

func TestWebSocketFrameEncodingMasksClientPayload(t *testing.T) {
	payload := []byte(`{"type":"req"}`)
	frame, err := encodeClientWebSocketFrame(1, payload)
	if err != nil {
		t.Fatalf("encodeClientWebSocketFrame returned error: %v", err)
	}
	if len(frame) < 6 || frame[0] != 0x81 || frame[1]&0x80 == 0 {
		t.Fatalf("client frame header = %#v", frame[:2])
	}
	reader := bufio.NewReader(bytes.NewReader(frame))
	opcode, decoded, err := readWebSocketFrame(reader)
	if err != nil {
		t.Fatalf("readWebSocketFrame returned error: %v", err)
	}
	if opcode != 1 || string(decoded) != string(payload) {
		t.Fatalf("opcode=%d decoded=%s", opcode, decoded)
	}
	if _, err := reader.Peek(1); err != io.EOF {
		t.Fatalf("trailing frame data err = %v", err)
	}
}

func TestWebSocketRejectsHostileServerFrames(t *testing.T) {
	tests := []struct {
		name      string
		frame     []byte
		violation webSocketProtocolViolation
	}{
		{name: "reserved-bits", frame: []byte{0xc1, 0x00}, violation: webSocketReservedBits},
		{name: "fragmented", frame: []byte{0x01, 0x00}, violation: webSocketFragmentation},
		{name: "masked-server-frame", frame: []byte{0x81, 0x80, 0, 0, 0, 0}, violation: webSocketMaskDirection},
		{name: "invalid-opcode", frame: []byte{0x83, 0x00}, violation: webSocketInvalidOpcode},
		{name: "truncated-header", frame: []byte{0x81}, violation: webSocketInvalidFraming},
		{name: "non-minimal-16-bit-length", frame: []byte{0x81, 126, 0, 1, 'x'}, violation: webSocketInvalidFraming},
		{name: "non-minimal-64-bit-length", frame: []byte{0x81, 127, 0, 0, 0, 0, 0, 0, 0, 1, 'x'}, violation: webSocketInvalidFraming},
		{name: "oversized-message", frame: []byte{0x81, 0x7f, 0, 0, 0, 0, 4, 0, 0, 1}, violation: webSocketMessageTooLarge},
		{name: "oversized-control", frame: []byte{0x89, 126}, violation: webSocketInvalidControl},
		{name: "invalid-text", frame: []byte{0x81, 0x01, 0xff}, violation: webSocketInvalidFraming},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := readWebSocketFrameWithMask(bufio.NewReader(bytes.NewReader(test.frame)), false)
			var protocolErr *webSocketProtocolError
			if !errors.As(err, &protocolErr) || protocolErr.violation != test.violation {
				t.Fatalf("protocol error = %#v, want %q", err, test.violation)
			}
		})
	}
}

func TestWebSocketRejectsInvalidCloseFrames(t *testing.T) {
	tests := [][]byte{
		{0},
		{0, 1},
		{0x03, 0xed},
		{0x07, 0xd0},
		{0x03, 0xe8, 0xff},
	}
	for _, payload := range tests {
		err := validateClosePayload(payload)
		var protocolErr *webSocketProtocolError
		if !errors.As(err, &protocolErr) || protocolErr.violation != webSocketInvalidClose {
			t.Fatalf("close payload %x error = %#v", payload, err)
		}
	}
	for _, payload := range [][]byte{nil, {0x03, 0xe8}, {0x0b, 0xb8, 'o', 'k'}} {
		if err := validateClosePayload(payload); err != nil {
			t.Fatalf("valid close payload %x rejected: %v", payload, err)
		}
	}
}

func TestClientWebSocketProtocolViolationDominatesQueuedDelivery(t *testing.T) {
	t.Run("response", func(t *testing.T) {
		reader, writer := io.Pipe()
		websocket := &webSocketTransport{connection: discardNetConn{}, reader: bufio.NewReader(reader)}
		release := make(chan struct{})
		transport := &heldSendTransport{Transport: websocket, sent: make(chan []byte, 1), release: release}
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
		if err != nil {
			t.Fatal(err)
		}
		client.transport = transport
		go client.readLoop(context.Background(), transport)

		done := make(chan error, 1)
		go func() { _, err := client.Call(context.Background(), "status", nil); done <- err }()
		request := decodeGatewayFrame(t, <-transport.sent)
		response, err := json.Marshal(Frame{"type": "res", "id": request.String("id"), "ok": true})
		if err != nil {
			t.Fatal(err)
		}
		writeDone := make(chan error, 1)
		go func() {
			_, err := writer.Write(append(encodeServerTextFrame(response), 0xc1, 0x00))
			writeDone <- err
		}()
		fatal := waitForFatalError(t, client)
		close(release)
		if err := <-done; !errors.Is(err, fatal) {
			t.Fatalf("Call accepted queued response over protocol failure: %v", err)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("event", func(t *testing.T) {
		event, err := json.Marshal(Frame{"type": "event", "event": "partial", "payload": map[string]any{"text": "safe"}})
		if err != nil {
			t.Fatal(err)
		}
		secret := "auth-secret-must-not-leak"
		hostile := append([]byte{0xc1, byte(len(secret))}, secret...)
		content := append(encodeServerTextFrame(event), hostile...)
		transport := &webSocketTransport{connection: discardNetConn{}, reader: bufio.NewReader(bytes.NewReader(content))}
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
		if err != nil {
			t.Fatal(err)
		}
		client.transport = transport
		go client.readLoop(context.Background(), transport)
		fatal := waitForFatalError(t, client)
		if strings.Contains(fatal.Error(), secret) {
			t.Fatalf("protocol error leaked hostile frame content: %v", fatal)
		}
		frame, err := client.RecvEvent(context.Background())
		if frame != nil || !errors.Is(err, fatal) {
			t.Fatalf("RecvEvent accepted queued event %#v over protocol failure: %v", frame, err)
		}
	})
}

func TestClientWebSocketEOFLeavesQueuedCompleteEventNonfatal(t *testing.T) {
	event, err := json.Marshal(Frame{"type": "event", "event": "complete"})
	if err != nil {
		t.Fatal(err)
	}
	transport := &webSocketTransport{connection: discardNetConn{}, reader: bufio.NewReader(bytes.NewReader(encodeServerTextFrame(event)))}
	client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
	if err != nil {
		t.Fatal(err)
	}
	client.transport = transport
	client.readLoop(context.Background(), transport)
	if err := client.FatalError(); err != nil {
		t.Fatalf("normal EOF reported fatal: %v", err)
	}
	frame, err := client.RecvEvent(context.Background())
	if err != nil || frame.String("event") != "complete" {
		t.Fatalf("queued complete event = %#v, %v", frame, err)
	}
}

func TestWebSocketCloseUnblocksWritesWithoutWaitingForWriterLock(t *testing.T) {
	t.Run("direct-and-repeated", func(t *testing.T) {
		connection := newBlockingWriteConn()
		transport := &webSocketTransport{connection: connection, reader: bufio.NewReader(connection)}
		done := make(chan error, 1)
		go func() { done <- transport.Close() }()
		if err := waitForClose(t, connection, done); err != nil {
			t.Fatal(err)
		}
		select {
		case <-connection.writeStarted:
			t.Fatal("Close attempted a graceful control-frame write before closing the connection")
		default:
		}
		if err := transport.Close(); err != nil {
			t.Fatalf("repeated Close returned error: %v", err)
		}
	})

	tests := []struct {
		name   string
		reader []byte
		write  func(*webSocketTransport) error
	}{
		{name: "normal-write", write: func(transport *webSocketTransport) error {
			return transport.Send(context.Background(), []byte("request"))
		}},
		{name: "pong-control-write", reader: []byte{0x89, 0x04, 'p', 'i', 'n', 'g'}, write: func(transport *webSocketTransport) error {
			_, err := transport.Receive(context.Background())
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection := newBlockingWriteConn()
			reader := io.Reader(connection)
			if test.reader != nil {
				reader = bytes.NewReader(test.reader)
			}
			transport := &webSocketTransport{connection: connection, reader: bufio.NewReader(reader)}
			writeDone := make(chan error, 1)
			go func() { writeDone <- test.write(transport) }()
			select {
			case <-connection.writeStarted:
			case <-time.After(time.Second):
				t.Fatal("write did not start")
			}
			closeDone := make(chan error, 1)
			go func() { closeDone <- transport.Close() }()
			if err := waitForClose(t, connection, closeDone); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-writeDone:
				if err == nil || !errors.Is(err, net.ErrClosed) {
					t.Fatalf("blocked write error = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked write goroutine was not released")
			}
		})
	}
}

func waitForClose(t *testing.T, connection *blockingWriteConn, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(100 * time.Millisecond):
		connection.forceClose()
		<-done
		t.Fatal("Close waited for a blocked WebSocket write")
		return nil
	}
}

func waitForFatalError(t *testing.T, client *Client) error {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := client.FatalError(); err != nil {
			if !isWebSocketProtocolError(err) {
				t.Fatalf("fatal error is not classified: %v", err)
			}
			if strings.Contains(err.Error(), "safe") {
				t.Fatalf("protocol error leaked frame content: %v", err)
			}
			return err
		}
		time.Sleep(time.Microsecond)
	}
	t.Fatal("protocol violation did not become fatal")
	return nil
}

func decodeGatewayFrame(t *testing.T, content []byte) Frame {
	t.Helper()
	var frame Frame
	if err := json.Unmarshal(content, &frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

type heldSendTransport struct {
	Transport
	sent    chan []byte
	release <-chan struct{}
}

func (transport *heldSendTransport) Send(ctx context.Context, content []byte) error {
	select {
	case transport.sent <- append([]byte(nil), content...):
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-transport.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type discardNetConn struct{}

func (discardNetConn) Read([]byte) (int, error)          { return 0, io.EOF }
func (discardNetConn) Write(content []byte) (int, error) { return len(content), nil }
func (discardNetConn) Close() error                      { return nil }
func (discardNetConn) LocalAddr() net.Addr               { return discardAddr("local") }
func (discardNetConn) RemoteAddr() net.Addr              { return discardAddr("remote") }
func (discardNetConn) SetDeadline(time.Time) error       { return nil }
func (discardNetConn) SetReadDeadline(time.Time) error   { return nil }
func (discardNetConn) SetWriteDeadline(time.Time) error  { return nil }

type discardAddr string

func (address discardAddr) Network() string { return "discard" }
func (address discardAddr) String() string  { return string(address) }

type blockingWriteConn struct {
	writeStarted chan struct{}
	closed       chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
}

func newBlockingWriteConn() *blockingWriteConn {
	return &blockingWriteConn{writeStarted: make(chan struct{}), closed: make(chan struct{})}
}

func (connection *blockingWriteConn) Read([]byte) (int, error) {
	<-connection.closed
	return 0, net.ErrClosed
}

func (connection *blockingWriteConn) Write([]byte) (int, error) {
	connection.writeOnce.Do(func() { close(connection.writeStarted) })
	<-connection.closed
	return 0, net.ErrClosed
}

func (connection *blockingWriteConn) Close() error {
	connection.forceClose()
	return nil
}

func (connection *blockingWriteConn) forceClose() {
	connection.closeOnce.Do(func() { close(connection.closed) })
}

func (*blockingWriteConn) LocalAddr() net.Addr              { return discardAddr("local") }
func (*blockingWriteConn) RemoteAddr() net.Addr             { return discardAddr("remote") }
func (*blockingWriteConn) SetDeadline(time.Time) error      { return nil }
func (*blockingWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (*blockingWriteConn) SetWriteDeadline(time.Time) error { return nil }
