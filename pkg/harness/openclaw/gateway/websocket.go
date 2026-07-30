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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type WebSocketOptions struct {
	URL string
}

type webSocketTransport struct {
	connection net.Conn
	reader     *bufio.Reader
	mu         sync.Mutex
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
		opcode, payload, err := readWebSocketFrame(transport.reader)
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 1, 2:
			return payload, nil
		case 8:
			return nil, io.EOF
		case 9:
			if err := transport.writeControl(10, payload); err != nil {
				return nil, err
			}
		case 10:
		default:
			return nil, fmt.Errorf("gateway websocket returned unsupported opcode %d", opcode)
		}
	}
}

func (transport *webSocketTransport) Close() error {
	if transport == nil || transport.connection == nil {
		return nil
	}
	_ = transport.writeControl(8, []byte{})
	return transport.connection.Close()
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
	header, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	if header&0x70 != 0 {
		return 0, nil, fmt.Errorf("gateway websocket frame has reserved bits set")
	}
	if header&0x80 == 0 {
		return 0, nil, fmt.Errorf("gateway websocket fragmented frames are not supported")
	}
	opcode := header & 0x0f
	lengthByte, err := reader.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	masked := lengthByte&0x80 != 0
	length := uint64(lengthByte & 0x7f)
	switch length {
	case 126:
		var encoded [2]byte
		if _, err := io.ReadFull(reader, encoded[:]); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(encoded[:]))
	case 127:
		var encoded [8]byte
		if _, err := io.ReadFull(reader, encoded[:]); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(encoded[:])
	}
	if length > maxMessageBytes {
		return 0, nil, fmt.Errorf("gateway websocket message too large: %s bytes", strconv.FormatUint(length, 10))
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index%len(mask)]
		}
	}
	return opcode, payload, nil
}
