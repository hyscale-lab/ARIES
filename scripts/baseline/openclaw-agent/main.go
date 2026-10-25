// openclaw-agent submits one task to an OpenClaw gateway through the client
// ARIES uses, and prints the run's identity and reply as JSON. The native
// baseline runs it inside the gateway's container, so a task reaches OpenClaw
// as it does under ARIES, without the openclaw CLI's startup.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	gatewayclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
)

func main() {
	url := flag.String("url", "ws://127.0.0.1:18789", "gateway WebSocket URL")
	tokenEnv := flag.String("token-env", "OPENCLAW_GATEWAY_TOKEN", "environment variable holding the gateway token")
	messageFile := flag.String("message-file", "", "file holding the task instruction")
	sessionKey := flag.String("session-key", "", "OpenClaw session key")
	thinking := flag.String("thinking", "low", "thinking level, as ARIES requests it")
	timeout := flag.Duration("timeout", time.Hour, "limit on the whole run")
	flag.Parse()
	if err := run(*url, os.Getenv(*tokenEnv), *messageFile, *sessionKey, *thinking, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "openclaw-agent:", err)
		os.Exit(1)
	}
}

func run(url, token, messageFile, sessionKey, thinking string, timeout time.Duration) error {
	if token == "" || messageFile == "" || sessionKey == "" {
		return errors.New("a gateway token, -message-file and -session-key are required")
	}
	message, err := os.ReadFile(messageFile)
	if err != nil {
		return err
	}
	var key [12]byte
	if _, err := rand.Read(key[:]); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	dialer, err := gatewayclient.NewWebSocketDialer(gatewayclient.WebSocketOptions{URL: url})
	if err != nil {
		return err
	}
	client, err := gatewayclient.New(dialer, gatewayclient.Options{Token: token, Scopes: []string{"operator.write"}, EventDisposition: gatewayclient.EventDispositionResponseOnly})
	if err != nil {
		return err
	}
	defer client.Close()
	summary, err := client.Connect(ctx, gatewayclient.ConnectOptions{})
	if err != nil {
		return err
	}
	if !summary.HasScope("operator.write") {
		return errors.New("gateway did not grant operator.write")
	}
	started := time.Now()
	result, err := client.Agent(ctx, gatewayclient.AgentRequest{
		Message: string(message), SessionKey: sessionKey,
		IdempotencyKey: hex.EncodeToString(key[:]), Thinking: thinking,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		RunID      string `json:"run_id"`
		Text       string `json:"text"`
		DurationMS int64  `json:"duration_ms"`
	}{result.RunID, result.Text, time.Since(started).Milliseconds()})
}
