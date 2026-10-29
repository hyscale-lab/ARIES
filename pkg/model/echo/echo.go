// Package echo implements a deterministic OpenAI-compatible server for testing
// a harness's real request path without a model. By default it echoes the
// request; an explicit tool command exercises a complete exec tool round trip.
package echo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultModel is the id served by /v1/models when Options.Model is empty.
	DefaultModel    = "aries-echo"
	maxRequestBytes = 16 << 20
	redacted        = "[redacted]"
)

// Options configures a Server. An empty LogPath disables the request log.
type Options struct {
	Model   string
	LogPath string
	// ToolCommand makes each user turn issue one exec call, then return its
	// tool result. Empty preserves the request-echo behavior.
	ToolCommand string
}

// Record is one received request. It is both the assistant message content and
// one line of the optional JSONL request log. Credential headers are replaced
// by a placeholder so neither the reply nor the log can leak a model key.
type Record struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query,omitempty"`
	Headers map[string][]string `json:"headers"`
	Body    json.RawMessage     `json:"body,omitempty"`
}

// Server is an http.Handler. Close releases the request log.
type Server struct {
	model       string
	toolCommand string

	mu  sync.Mutex
	log *os.File
}

func New(options Options) (*Server, error) {
	if strings.ContainsRune(options.ToolCommand, 0) {
		return nil, fmt.Errorf("tool command contains NUL")
	}
	server := &Server{model: options.Model, toolCommand: options.ToolCommand}
	if server.model == "" {
		server.model = DefaultModel
	}
	if options.LogPath != "" {
		log, err := os.OpenFile(options.LogPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open echo request log: %w", err)
		}
		server.log = log
	}
	return server, nil
}

func (server *Server) Close() error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.log == nil {
		return nil
	}
	err := server.log.Close()
	server.log = nil
	return err
}

func (server *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "read request: "+err.Error())
		return
	}
	record := newRecord(r, body)
	if err := server.append(record); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		writeJSON(w, map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": server.model, "object": "model", "created": 0, "owned_by": "aries"}},
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		server.chat(w, record)
	default:
		writeError(w, http.StatusNotFound, "unsupported route "+r.Method+" "+r.URL.Path)
	}
}

func newRecord(r *http.Request, body []byte) Record {
	headers := make(map[string][]string, len(r.Header))
	for name, values := range r.Header {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "x-api-key", "api-key", "cookie":
			headers[name] = []string{redacted}
		default:
			headers[name] = values
		}
	}
	record := Record{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Headers: headers}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 {
		if json.Valid(trimmed) {
			record.Body = json.RawMessage(trimmed)
		} else {
			record.Body, _ = json.Marshal(string(body))
		}
	}
	return record
}

func (server *Server) append(record Record) error {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.log == nil {
		return nil
	}
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := server.log.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write echo request log: %w", err)
	}
	return nil
}

func (server *Server) chat(w http.ResponseWriter, record Record) {
	var request struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		Messages []chatMessage `json:"messages"`
		Tools    []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(record.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, "chat completion body must be a JSON object")
		return
	}
	message := map[string]any{"role": "assistant"}
	finishReason := "stop"
	if server.toolCommand == "" {
		content, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		message["content"] = string(content)
	} else {
		// OpenClaw normalizes tool IDs to alphanumeric characters. Emit that
		// native spelling and give each user turn its own stable identifier.
		userTurns := 0
		for _, message := range request.Messages {
			if message.Role == "user" {
				userTurns++
			}
		}
		callID := fmt.Sprintf("callariesechoexec%d", userTurns)
		// History belongs to the request, not to server-global state. Parallel
		// conversations each get one call and replaying a request is stable.
		var result *chatMessage
		for i := len(request.Messages) - 1; i >= 0; i-- {
			m := &request.Messages[i]
			if m.Role == "user" {
				break
			}
			if m.Role == "tool" && m.ToolCallID == callID {
				result = m
				break
			}
		}
		if result != nil {
			var output string
			if err := json.Unmarshal(result.Content, &output); err != nil {
				output = string(result.Content)
			}
			message["content"] = "Mock exec tool result:\n" + output
		} else {
			execOffered := false
			for _, tool := range request.Tools {
				if tool.Type == "function" && tool.Function.Name == "exec" {
					execOffered = true
					break
				}
			}
			if !execOffered {
				writeError(w, http.StatusBadRequest, "tool-command mode requires an offered exec function")
				return
			}
			arguments, _ := json.Marshal(map[string]string{"command": server.toolCommand})
			message["content"] = nil
			message["tool_calls"] = []map[string]any{{"id": callID, "type": "function", "function": map[string]any{"name": "exec", "arguments": string(arguments)}}}
			finishReason = "tool_calls"
		}
	}
	model := request.Model
	if model == "" {
		model = server.model
	}
	id := fmt.Sprintf("chatcmpl-aries-echo-%d", time.Now().UnixNano())
	usage := map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	if !request.Stream {
		writeJSON(w, map[string]any{
			"id": id, "object": "chat.completion", "created": 0, "model": model,
			"choices": []map[string]any{{
				"index": 0, "finish_reason": finishReason,
				"message": message,
			}},
			"usage": usage,
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	chunk := func(choices []map[string]any, usage any) {
		payload := map[string]any{"id": id, "object": "chat.completion.chunk", "created": 0, "model": model, "choices": choices}
		if usage != nil {
			payload["usage"] = usage
		}
		line, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	if calls, ok := message["tool_calls"].([]map[string]any); ok {
		for index, call := range calls {
			call["index"] = index
		}
	}
	chunk([]map[string]any{{"index": 0, "delta": message, "finish_reason": nil}}, nil)
	chunk([]map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}}, nil)
	if request.StreamOptions.IncludeUsage {
		chunk([]map[string]any{}, usage)
	}
	io.WriteString(w, "data: [DONE]\n\n")
}

type chatMessage struct {
	Role       string          `json:"role"`
	ToolCallID string          `json:"tool_call_id"`
	Content    json.RawMessage `json:"content"`
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "aries_echo_error"}})
}
