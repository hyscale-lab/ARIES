package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	tests := map[string][]byte{
		"fragmented": {0x01, 0x00},
		"masked":     {0x81, 0x80, 0, 0, 0, 0},
		"oversized":  {0x81, 0x7f, 0, 0, 0, 0, 4, 0, 0, 1},
	}
	for name, frame := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := readWebSocketFrameWithMask(bufio.NewReader(bytes.NewReader(frame)), false); err == nil {
				t.Fatal("hostile frame was accepted")
			}
		})
	}
}
