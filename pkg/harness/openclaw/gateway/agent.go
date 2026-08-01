package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const agentResponseQueueSize = 4
const maxAgentResultBytes = 16 << 20

type AgentRequest struct {
	Message        string
	SessionKey     string
	IdempotencyKey string
	Thinking       string
}

type AgentResult struct {
	RunID string
	Text  string
}

func (client *Client) Agent(ctx context.Context, request AgentRequest) (AgentResult, error) {
	if strings.TrimSpace(request.Message) == "" || strings.ContainsRune(request.Message, 0) {
		return AgentResult{}, errors.New("gateway agent message is invalid")
	}
	if strings.TrimSpace(request.SessionKey) == "" {
		return AgentResult{}, errors.New("gateway agent session key is required")
	}
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return AgentResult{}, errors.New("gateway agent idempotency key is required")
	}
	client.mu.Lock()
	summary := client.connected
	transport := client.transport
	client.mu.Unlock()
	if summary == nil || transport == nil {
		return AgentResult{}, errors.New("gateway agent client is not connected")
	}
	if !summary.HasScope("operator.write") {
		return AgentResult{}, errors.New("gateway agent requires operator.write scope")
	}

	id := client.nextID("agent")
	reply := make(chan responseDelivery, agentResponseQueueSize)
	client.mu.Lock()
	if client.transport != transport {
		client.mu.Unlock()
		return AgentResult{}, errors.New("gateway agent transport changed before submission")
	}
	client.pending[id] = reply
	client.mu.Unlock()
	defer client.removePending(id)

	params := map[string]any{
		"message": request.Message, "sessionKey": request.SessionKey,
		"idempotencyKey": request.IdempotencyKey,
	}
	if request.Thinking != "" {
		params["thinking"] = request.Thinking
	}
	content, err := json.Marshal(Frame{"type": "req", "id": id, "method": "agent", "params": params})
	if err != nil {
		return AgentResult{}, fmt.Errorf("marshal gateway agent request: %w", err)
	}
	if err := transport.Send(ctx, content); err != nil {
		return AgentResult{}, fmt.Errorf("gateway agent delivery is ambiguous after send failure: %w", err)
	}

	acceptedRunID := ""
	for {
		select {
		case delivery := <-reply:
			if delivery.err != nil {
				if acceptedRunID == "" {
					return AgentResult{}, fmt.Errorf("gateway agent delivery is ambiguous before acknowledgement: %w", delivery.err)
				}
				return AgentResult{}, fmt.Errorf("gateway agent %s was accepted but its outcome is unknown: %w", acceptedRunID, delivery.err)
			}
			response := delivery.frame
			if response.String("id") != id || response.String("type") != "res" {
				return AgentResult{}, errors.New("gateway agent received an uncorrelated response")
			}
			if !response.Bool("ok") {
				return AgentResult{}, agentProtocolError(response)
			}
			payload := response.Map("payload")
			runID, _ := payload["runId"].(string)
			status, _ := payload["status"].(string)
			if acceptedRunID == "" {
				if status != "accepted" || strings.TrimSpace(runID) == "" {
					return AgentResult{}, errors.New("gateway agent expected accepted response with non-empty runId")
				}
				acceptedRunID = runID
				continue
			}
			if strings.TrimSpace(runID) == "" || runID != acceptedRunID {
				return AgentResult{}, errors.New("gateway agent terminal runId did not match accepted runId")
			}
			text, err := parseAgentTerminal(payload)
			if err != nil {
				return AgentResult{}, err
			}
			return AgentResult{RunID: runID, Text: text}, nil
		case <-ctx.Done():
			if acceptedRunID == "" {
				return AgentResult{}, fmt.Errorf("gateway agent delivery is ambiguous before acknowledgement: %w", ctx.Err())
			}
			return AgentResult{}, fmt.Errorf("gateway agent %s was accepted but its outcome is unknown: %w", acceptedRunID, ctx.Err())
		}
	}
}

func agentProtocolError(response Frame) error {
	errorPayload := response.Map("error")
	code := boundedDiagnostic(errorPayload["code"])
	message := boundedDiagnostic(errorPayload["message"])
	if code == "" {
		code = "UNKNOWN"
	}
	if message == "" {
		message = "request rejected"
	}
	return fmt.Errorf("gateway agent rejected request (%s): %s", code, message)
}

func boundedDiagnostic(value any) string {
	text, _ := value.(string)
	text = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, text)
	text = strings.TrimSpace(text)
	if len(text) > 512 {
		text = text[:512] + "…"
	}
	return text
}

func parseAgentTerminal(payload map[string]any) (string, error) {
	status, _ := payload["status"].(string)
	if status != "ok" {
		return "", fmt.Errorf("gateway agent terminal status is %q, want %q", status, "ok")
	}
	result, ok := payload["result"].(map[string]any)
	if !ok {
		return "", errors.New("gateway agent terminal response missing result")
	}
	rawPayloads, ok := result["payloads"].([]any)
	if !ok {
		return "", errors.New("gateway agent terminal result missing payloads")
	}
	texts := make([]string, 0, len(rawPayloads))
	for _, raw := range rawPayloads {
		payload, ok := raw.(map[string]any)
		if !ok {
			return "", errors.New("gateway agent terminal result contains malformed payload")
		}
		text, ok := payload["text"].(string)
		if !ok {
			return "", errors.New("gateway agent terminal result payload missing text")
		}
		if text != "" {
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return "", errors.New("gateway agent returned no payload text")
	}
	joined := strings.Join(texts, "\n")
	if len(joined) > maxAgentResultBytes {
		return "", errors.New("gateway agent result exceeded size bound")
	}
	return joined, nil
}
