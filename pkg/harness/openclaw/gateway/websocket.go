package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

var errWebSocketProtocol = errors.New("gateway websocket protocol violation")

func isWebSocketProtocolError(err error) bool {
	return errors.Is(err, errWebSocketProtocol)
}

type WebSocketOptions struct {
	URL string
}

type webSocketTransport struct {
	connection *websocket.Conn
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
		return nil, errors.New("invalid gateway websocket URL")
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("gateway websocket URL must use ws or wss")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("gateway websocket URL missing host")
	}
	return func(ctx context.Context) (Transport, error) {
		// DialContext bounds connection establishment and TLS. Closing the socket
		// also interrupts an HTTP upgrade when cancellation has no deadline.
		stop := func() bool { return true }
		dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
			if err == nil {
				stop = context.AfterFunc(ctx, func() { _ = connection.Close() })
			}
			return connection, err
		}}
		connection, response, err := dialer.DialContext(ctx, rawURL, nil)
		stop()
		if response != nil {
			_ = response.Body.Close()
		}
		if ctx.Err() != nil {
			if connection != nil {
				_ = connection.Close()
			}
			return nil, ctx.Err()
		}
		if err != nil {
			// Upgrade responses and URL errors may contain credentials.
			return nil, errors.New("gateway websocket handshake failed")
		}
		connection.SetReadLimit(maxMessageBytes)
		return &webSocketTransport{connection: connection}, nil
	}, nil
}

func (transport *webSocketTransport) Send(ctx context.Context, content []byte) error {
	if transport == nil || transport.connection == nil {
		return errors.New("gateway websocket is not connected")
	}
	if len(content) > maxMessageBytes {
		return errors.New("gateway websocket message too large")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = transport.Close() })
	defer stop()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := transport.connection.WriteMessage(websocket.TextMessage, content); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("write gateway websocket message failed")
	}
	return nil
}

func (transport *webSocketTransport) Receive(ctx context.Context) ([]byte, error) {
	if transport == nil || transport.connection == nil {
		return nil, errors.New("gateway websocket is not connected")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = transport.Close() })
	defer stop()
	typ, reader, err := transport.connection.NextReader()
	var content []byte
	if err == nil {
		content, err = io.ReadAll(reader)
		if err != nil {
			// A connection lost inside a message cannot validate queued results.
			err = errWebSocketProtocol
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		// Close reasons and library protocol diagnostics must not expose peer
		// content. Ordinary disconnections still allow queued complete events.
		var closeErr *websocket.CloseError
		var netErr net.Error
		if errors.As(err, &closeErr) || errors.As(err, &netErr) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, errWebSocketProtocol
	}
	// Gorilla leaves text encoding validation to the application. Validate the
	// assembled message so UTF-8 characters may span continuation frames.
	if typ == websocket.TextMessage && !utf8.Valid(content) {
		return nil, errWebSocketProtocol
	}
	return content, nil
}

func (transport *webSocketTransport) Close() error {
	if transport == nil || transport.connection == nil {
		return nil
	}
	transport.closeOnce.Do(func() {
		// Close releases blocked data/control writes without a close handshake.
		transport.closeErr = transport.connection.Close()
	})
	return transport.closeErr
}
