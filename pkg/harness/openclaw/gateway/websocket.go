package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type webSocketProtocolViolation string

const (
	webSocketReservedBits    webSocketProtocolViolation = "reserved bits"
	webSocketFragmentation   webSocketProtocolViolation = "fragmentation"
	webSocketInvalidOpcode   webSocketProtocolViolation = "invalid opcode"
	webSocketMaskDirection   webSocketProtocolViolation = "invalid mask direction"
	webSocketInvalidFraming  webSocketProtocolViolation = "invalid framing"
	webSocketMessageTooLarge webSocketProtocolViolation = "message too large"
	webSocketInvalidControl  webSocketProtocolViolation = "invalid control frame"
	webSocketInvalidClose    webSocketProtocolViolation = "invalid close frame"
)

type webSocketProtocolError struct {
	violation webSocketProtocolViolation
}

func (err *webSocketProtocolError) Error() string {
	return "gateway websocket protocol violation: " + string(err.violation)
}

func newWebSocketProtocolError(violation webSocketProtocolViolation) error {
	return &webSocketProtocolError{violation: violation}
}

func isWebSocketProtocolError(err error) bool {
	var protocolErr *webSocketProtocolError
	return errors.As(err, &protocolErr)
}

type WebSocketOptions struct {
	URL string
}

type webSocketTransport struct {
	connection net.Conn
	reader     *bufio.Reader
	mu         sync.Mutex
	closeOnce  sync.Once
	closeErr   error
}

func NewWebSocketDialer(options WebSocketOptions) (DialFunc, error) {
	rawURL := strings.TrimSpace(options.URL)
	if rawURL == "" {
		return nil, fmt.Errorf("gateway websocket URL is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse gateway websocket URL: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("gateway websocket URL must use ws or wss")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("gateway websocket URL missing host")
	}
	return func(ctx context.Context) (Transport, error) {
		return dialWebSocket(ctx, parsed)
	}, nil
}

func dialWebSocket(ctx context.Context, endpoint *url.URL) (*webSocketTransport, error) {
	connection, err := dialWebSocketConnection(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	key, err := websocketKey()
	if err != nil {
		connection.Close()
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		connection.SetDeadline(deadline)
		defer connection.SetDeadline(time.Time{})
	}
	path := endpoint.RequestURI()
	if path == "" {
		path = "/"
	}
	request := bytes.NewBuffer(nil)
	fmt.Fprintf(request, "GET %s HTTP/1.1\r\n", path)
	fmt.Fprintf(request, "Host: %s\r\n", endpoint.Host)
	fmt.Fprintf(request, "Upgrade: websocket\r\n")
	fmt.Fprintf(request, "Connection: Upgrade\r\n")
	fmt.Fprintf(request, "Sec-WebSocket-Key: %s\r\n", key)
	fmt.Fprintf(request, "Sec-WebSocket-Version: 13\r\n")
	fmt.Fprintf(request, "\r\n")
	if _, err := connection.Write(request.Bytes()); err != nil {
		connection.Close()
		return nil, fmt.Errorf("write gateway websocket handshake: %w", err)
	}
	reader := bufio.NewReader(connection)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		connection.Close()
		return nil, fmt.Errorf("read gateway websocket handshake: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		connection.Close()
		return nil, fmt.Errorf("gateway websocket handshake returned %s", response.Status)
	}
	if !headerToken(response.Header, "Upgrade", "websocket") || !headerToken(response.Header, "Connection", "upgrade") {
		connection.Close()
		return nil, fmt.Errorf("gateway websocket handshake missing upgrade headers")
	}
	if response.Header.Get("Sec-WebSocket-Accept") != websocketAccept(key) {
		connection.Close()
		return nil, fmt.Errorf("gateway websocket handshake returned invalid accept key")
	}
	return &webSocketTransport{connection: connection, reader: reader}, nil
}

func dialWebSocketConnection(ctx context.Context, endpoint *url.URL) (net.Conn, error) {
	address := endpoint.Host
	if endpoint.Port() == "" {
		if endpoint.Scheme == "wss" {
			address = net.JoinHostPort(endpoint.Hostname(), "443")
		} else {
			address = net.JoinHostPort(endpoint.Hostname(), "80")
		}
	}
	dialer := net.Dialer{}
	if endpoint.Scheme != "wss" {
		connection, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, fmt.Errorf("dial gateway websocket: %w", err)
		}
		return connection, nil
	}
	tlsDialer := tls.Dialer{NetDialer: &dialer, Config: &tls.Config{ServerName: endpoint.Hostname()}}
	connection, err := tlsDialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial gateway websocket TLS: %w", err)
	}
	return connection, nil
}

func (transport *webSocketTransport) Send(ctx context.Context, content []byte) error {
	if transport == nil || transport.connection == nil {
		return fmt.Errorf("gateway websocket is not connected")
	}
	frame, err := encodeClientWebSocketFrame(1, content)
	if err != nil {
		return err
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if deadline, ok := ctx.Deadline(); ok {
		transport.connection.SetWriteDeadline(deadline)
		defer transport.connection.SetWriteDeadline(time.Time{})
	}
	if _, err := transport.connection.Write(frame); err != nil {
		return fmt.Errorf("write gateway websocket frame: %w", err)
	}
	return nil
}

func (transport *webSocketTransport) Receive(ctx context.Context) ([]byte, error) {
	if transport == nil || transport.connection == nil {
		return nil, fmt.Errorf("gateway websocket is not connected")
	}
	if deadline, ok := ctx.Deadline(); ok {
		transport.connection.SetReadDeadline(deadline)
		defer transport.connection.SetReadDeadline(time.Time{})
	}
	for {
		opcode, payload, err := readWebSocketFrameWithMask(transport.reader, false)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		switch opcode {
		case 1, 2:
			return payload, nil
		case 8:
			if err := validateClosePayload(payload); err != nil {
				return nil, err
			}
			return nil, io.EOF
		case 9:
			if err := transport.writeControl(10, payload); err != nil {
				return nil, err
			}
		case 10:
		default:
			return nil, newWebSocketProtocolError(webSocketInvalidOpcode)
		}
	}
}

func (transport *webSocketTransport) Close() error {
	if transport == nil || transport.connection == nil {
		return nil
	}
	transport.closeOnce.Do(func() {
		transport.closeErr = transport.connection.Close()
	})
	return transport.closeErr
}

func (transport *webSocketTransport) writeControl(opcode byte, payload []byte) error {
	frame, err := encodeClientWebSocketFrame(opcode, payload)
	if err != nil {
		return err
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if _, err := transport.connection.Write(frame); err != nil {
		return fmt.Errorf("write gateway websocket control frame: %w", err)
	}
	return nil
}

func websocketKey() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate gateway websocket key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw[:]), nil
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + websocketGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func headerToken(headers http.Header, key, token string) bool {
	for _, value := range headers.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func encodeClientWebSocketFrame(opcode byte, payload []byte) ([]byte, error) {
	if int64(len(payload)) > maxMessageBytes {
		return nil, fmt.Errorf("gateway websocket message too large: %d bytes", len(payload))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return nil, fmt.Errorf("generate gateway websocket mask: %w", err)
	}
	frame := bytes.NewBuffer(nil)
	frame.WriteByte(0x80 | opcode)
	switch length := len(payload); {
	case length < 126:
		frame.WriteByte(0x80 | byte(length))
	case length <= 0xffff:
		frame.WriteByte(0x80 | 126)
		var encoded [2]byte
		binary.BigEndian.PutUint16(encoded[:], uint16(length))
		frame.Write(encoded[:])
	default:
		frame.WriteByte(0x80 | 127)
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], uint64(length))
		frame.Write(encoded[:])
	}
	frame.Write(mask[:])
	for index, value := range payload {
		frame.WriteByte(value ^ mask[index%len(mask)])
	}
	return frame.Bytes(), nil
}

func readWebSocketFrame(reader *bufio.Reader) (byte, []byte, error) {
	return readWebSocketFrameWithMask(reader, true)
}

func readWebSocketFrameWithMask(reader *bufio.Reader, expectMasked bool) (byte, []byte, error) {
	header, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if header&0x70 != 0 {
		return 0, nil, newWebSocketProtocolError(webSocketReservedBits)
	}
	if header&0x80 == 0 {
		return 0, nil, newWebSocketProtocolError(webSocketFragmentation)
	}
	opcode := header & 0x0f
	if opcode != 0x1 && opcode != 0x2 && opcode != 0x8 && opcode != 0x9 && opcode != 0xa {
		return 0, nil, newWebSocketProtocolError(webSocketInvalidOpcode)
	}
	lengthByte, err := reader.ReadByte()
	if err != nil {
		return 0, nil, newWebSocketProtocolError(webSocketInvalidFraming)
	}
	masked := lengthByte&0x80 != 0
	if masked != expectMasked {
		return 0, nil, newWebSocketProtocolError(webSocketMaskDirection)
	}
	length := uint64(lengthByte & 0x7f)
	if opcode >= 0x8 && length >= 126 {
		return 0, nil, newWebSocketProtocolError(webSocketInvalidControl)
	}
	switch length {
	case 126:
		var encoded [2]byte
		if _, err := io.ReadFull(reader, encoded[:]); err != nil {
			return 0, nil, newWebSocketProtocolError(webSocketInvalidFraming)
		}
		length = uint64(binary.BigEndian.Uint16(encoded[:]))
		if length < 126 {
			return 0, nil, newWebSocketProtocolError(webSocketInvalidFraming)
		}
	case 127:
		var encoded [8]byte
		if _, err := io.ReadFull(reader, encoded[:]); err != nil {
			return 0, nil, newWebSocketProtocolError(webSocketInvalidFraming)
		}
		length = binary.BigEndian.Uint64(encoded[:])
		if length < 65536 || length&(uint64(1)<<63) != 0 {
			return 0, nil, newWebSocketProtocolError(webSocketInvalidFraming)
		}
	}
	if length > maxMessageBytes {
		return 0, nil, newWebSocketProtocolError(webSocketMessageTooLarge)
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return 0, nil, newWebSocketProtocolError(webSocketInvalidFraming)
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, newWebSocketProtocolError(webSocketInvalidFraming)
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%len(mask)]
		}
	}
	if opcode == 1 && !utf8.Valid(payload) {
		return 0, nil, newWebSocketProtocolError(webSocketInvalidFraming)
	}
	return opcode, payload, nil
}

func validateClosePayload(payload []byte) error {
	if len(payload) == 1 {
		return newWebSocketProtocolError(webSocketInvalidClose)
	}
	if len(payload) < 2 {
		return nil
	}
	code := binary.BigEndian.Uint16(payload[:2])
	if !validCloseCode(code) || !utf8.Valid(payload[2:]) {
		return newWebSocketProtocolError(webSocketInvalidClose)
	}
	return nil
}

func validCloseCode(code uint16) bool {
	switch code {
	case 1000, 1001, 1002, 1003, 1007, 1008, 1009, 1010, 1011, 1012, 1013, 1014:
		return true
	default:
		return code >= 3000 && code < 5000
	}
}
