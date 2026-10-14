package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
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
	if !strings.EqualFold(request.Header.Get("Connection"), "upgrade") || !strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
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
	fmt.Fprintf(response, "Sec-WebSocket-Accept: %s\r\n", testWebSocketAccept(key))
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

// Raw peers exercise the library adapter independently, including messages
// fragmented at a UTF-8 boundary and control frames between continuations.
func TestWebSocketFragmentationAndPingPong(t *testing.T) {
	transport, peer, reader := newPipeWebSocket(t)
	want := []byte("hello 世界")
	serverDone := make(chan error, 1)
	go func() {
		first := encodeServerTextFrame(want[:7])
		first[0] = 0x01
		_, err := peer.Write(append(first, 0x89, 0x02, 'h', 'i'))
		if err != nil {
			serverDone <- err
			return
		}
		opcode, payload, err := readWebSocketFrame(reader)
		if err == nil && (opcode != 10 || string(payload) != "hi") {
			err = fmt.Errorf("pong = %d %q", opcode, payload)
		}
		if err == nil {
			last := encodeServerTextFrame(want[7:])
			last[0] = 0x80
			_, err = peer.Write(append([]byte{0x8a, 0}, last...))
		}
		serverDone <- err
	}()
	ctx, cancel := contextWithTestTimeout()
	defer cancel()
	got, err := transport.Receive(ctx)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Receive = %q, %v", got, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketReadLimitCoversAggregateFragments(t *testing.T) {
	transport, peer, reader := newPipeWebSocket(t)
	// A smaller limit exercises the same library setting without allocating 64 MiB.
	transport.connection.SetReadLimit(8)
	go func() {
		_, _ = peer.Write([]byte{0x01, 5, '1', '2', '3', '4', '5', 0x80, 4})
		_, _, _ = readWebSocketFrame(reader) // Consume the library's close frame.
	}()
	ctx, cancel := contextWithTestTimeout()
	defer cancel()
	if _, err := transport.Receive(ctx); !isWebSocketProtocolError(err) {
		t.Fatalf("oversized fragmented message error = %v", err)
	}
}

func TestWebSocketTruncatedMessageIsFatal(t *testing.T) {
	transport, peer, _ := newPipeWebSocket(t)
	go func() {
		_, _ = peer.Write([]byte{0x81, 4, 'a'})
		_ = peer.Close()
	}()
	ctx, cancel := contextWithTestTimeout()
	defer cancel()
	if content, err := transport.Receive(ctx); content != nil || !isWebSocketProtocolError(err) {
		t.Fatalf("truncated message = %q, %v", content, err)
	}
}

func TestWebSocketProtocolFailuresAreSecretFree(t *testing.T) {
	for _, frame := range [][]byte{
		{0xc1, 0},                // Reserved bit.
		{0x81, 0x80, 0, 0, 0, 0}, // Wrong mask direction.
		{0x83, 0},                // Unknown opcode.
		{0x09, 0},                // Fragmented ping.
		{0x81, 1, 0xff},          // Invalid UTF-8.
		{0x88, 3, 0, 1, 'x'},     // Invalid close code.
	} {
		t.Run(fmt.Sprintf("%x", frame), func(t *testing.T) {
			transport, peer, reader := newPipeWebSocket(t)
			go func() {
				_, _ = peer.Write(frame)
				_, _, _ = readWebSocketFrame(reader)
			}()
			ctx, cancel := contextWithTestTimeout()
			defer cancel()
			if _, err := transport.Receive(ctx); !isWebSocketProtocolError(err) || err.Error() != errWebSocketProtocol.Error() {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestClientWebSocketProtocolViolationDominatesQueuedDelivery(t *testing.T) {
	for _, kind := range []string{"response", "event"} {
		t.Run(kind, func(t *testing.T) {
			wire, peer, reader := newPipeWebSocket(t)
			release := make(chan struct{})
			transport := &heldSendTransport{Transport: wire, sent: make(chan []byte, 1), release: release}
			client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
			if err != nil {
				t.Fatal(err)
			}
			client.transport = transport
			go client.readLoop(context.Background(), transport)
			ctx, cancel := contextWithTestTimeout()
			defer cancel()
			done := make(chan error, 1)
			frame := Frame{"type": "event", "event": "partial"}
			if kind == "response" {
				go func() { _, err := client.Call(ctx, "status", nil); done <- err }()
				var request Frame
				if err := json.Unmarshal(<-transport.sent, &request); err != nil {
					t.Fatal(err)
				}
				frame = Frame{"type": "res", "id": request["id"], "ok": true}
			}
			content, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				_, _ = peer.Write(append(encodeServerTextFrame(content), 0xc1, 0))
				_, _, _ = readWebSocketFrame(reader)
			}()
			select {
			case <-client.readerFailed:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			fatal := client.FatalError()
			if !isWebSocketProtocolError(fatal) {
				t.Fatalf("fatal error = %v", fatal)
			}
			close(release)
			if kind == "response" {
				if err := <-done; !errors.Is(err, fatal) {
					t.Fatalf("queued response error = %v", err)
				}
			} else {
				if frame, err := client.RecvEvent(ctx); frame != nil || !errors.Is(err, fatal) {
					t.Fatalf("queued event = %#v, %v", frame, err)
				}
			}
		})
	}
}

func TestClientWebSocketEOFLeavesQueuedCompleteEventNonfatal(t *testing.T) {
	for _, closeFrame := range [][]byte{nil, append([]byte{0x88, 8, 0x03, 0xe8}, []byte("secret")...)} {
		transport, peer, reader := newPipeWebSocket(t)
		go func() {
			_, _ = peer.Write(append(encodeServerTextFrame([]byte(`{"type":"event","event":"complete"}`)), closeFrame...))
			if closeFrame != nil {
				_, _, _ = readWebSocketFrame(reader)
			}
			_ = peer.Close()
		}()
		client, err := New(func(context.Context) (Transport, error) { return transport, nil }, Options{})
		if err != nil {
			t.Fatal(err)
		}
		client.transport = transport
		ctx, cancel := contextWithTestTimeout()
		client.readLoop(ctx, transport)
		cancel()
		if err := client.FatalError(); err != nil {
			t.Fatalf("EOF reported fatal: %v", err)
		}
		frame, err := client.RecvEvent(context.Background())
		if err != nil || frame.String("event") != "complete" {
			t.Fatalf("queued event = %#v, %v", frame, err)
		}
		_, err = client.RecvEvent(context.Background())
		if !errors.Is(err, io.EOF) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("close error = %v", err)
		}
	}
}

func TestWebSocketCancellationAndCloseUnblockIO(t *testing.T) {
	for _, operation := range []string{"read", "write", "pong"} {
		for _, shutdown := range []string{"cancel", "close"} {
			t.Run(operation+"/"+shutdown, func(t *testing.T) {
				transport, peer, _ := newPipeWebSocket(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				started := make(chan struct{})
				go func() {
					if operation == "write" {
						done <- transport.Send(ctx, []byte("request"))
						return
					}
					close(started)
					_, err := transport.Receive(ctx)
					done <- err
				}()
				if operation == "pong" {
					if _, err := peer.Write([]byte{0x89, 1, 'x'}); err != nil {
						t.Fatal(err)
					}
				}
				if operation != "read" {
					// Read only a byte: the remaining write is blocked on net.Pipe.
					if _, err := peer.Read(make([]byte, 1)); err != nil {
						t.Fatal(err)
					}
				} else {
					<-started
				}
				if shutdown == "cancel" {
					cancel()
				} else {
					closed := make(chan error, 1)
					go func() { closed <- transport.Close() }()
					select {
					case err := <-closed:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(time.Second):
						t.Fatal("Close blocked")
					}
					if err := transport.Close(); err != nil {
						t.Fatalf("repeated Close: %v", err)
					}
				}
				select {
				case err := <-done:
					if err == nil || (shutdown == "cancel" && !errors.Is(err, context.Canceled)) {
						t.Fatalf("operation error = %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("operation remained blocked")
				}
			})
		}
	}
}

func TestWebSocketDialCancellationDuringUpgrade(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listener unavailable: %v", err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	dial, err := NewWebSocketDialer(WebSocketOptions{URL: "ws://" + listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := dial(ctx); done <- err }()
	connection := <-accepted
	defer connection.Close()
	if _, err := http.ReadRequest(bufio.NewReader(connection)); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("upgrade ignored cancellation")
	}
}

func newPipeWebSocket(t *testing.T) (*webSocketTransport, net.Conn, *bufio.Reader) {
	t.Helper()
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(peer)
	go func() {
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		_, _ = fmt.Fprintf(peer, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", testWebSocketAccept(request.Header.Get("Sec-WebSocket-Key")))
	}()
	dialer := websocket.Dialer{NetDialContext: func(context.Context, string, string) (net.Conn, error) { return client, nil }}
	ctx, cancel := contextWithTestTimeout()
	defer cancel()
	connection, response, err := dialer.DialContext(ctx, "ws://test", nil)
	if response != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	connection.SetReadLimit(maxMessageBytes)
	transport := &webSocketTransport{connection: connection}
	t.Cleanup(func() { _ = transport.Close() })
	return transport, peer, reader
}

func testWebSocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Only reads small masked client frames emitted by these tests.
func readWebSocketFrame(reader *bufio.Reader) (byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	if header[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame is not masked")
	}
	length := int(header[1] & 0x7f)
	if length == 126 {
		var encoded [2]byte
		if _, err := io.ReadFull(reader, encoded[:]); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(encoded[:]))
	} else if length == 127 {
		return 0, nil, errors.New("test peer only supports small messages")
	}
	var mask [4]byte
	if _, err := io.ReadFull(reader, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return header[0] & 0x0f, payload, nil
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
