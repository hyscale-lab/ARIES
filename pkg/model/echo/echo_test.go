package echo

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const toolRequest = `{"model":"m1","temperature":0.7,"max_tokens":5,"stream":%s,"stream_options":{"include_usage":true},
"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"exec","arguments":"{\"command\":\"ls\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"out"}],
"tools":[{"type":"function","function":{"name":"exec","parameters":{"type":"object"}}}]}`

func post(t *testing.T, handler http.Handler, stream string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?x=1", strings.NewReader(strings.Replace(toolRequest, "%s", stream, 1)))
	request.Header.Set("Authorization", "Bearer secret-key")
	request.Header.Set("X-Custom", "kept")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertEchoed(t *testing.T, content string) {
	t.Helper()
	if strings.Contains(content, "secret-key") {
		t.Fatalf("credential leaked into echo: %s", content)
	}
	var record Record
	if err := json.Unmarshal([]byte(content), &record); err != nil {
		t.Fatalf("content is not a record: %v\n%s", err, content)
	}
	var body struct {
		Temperature float64           `json:"temperature"`
		MaxTokens   int               `json:"max_tokens"`
		Messages    []json.RawMessage `json:"messages"`
		Tools       []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(record.Body, &body); err != nil {
		t.Fatal(err)
	}
	if record.Method != "POST" || record.Path != "/v1/chat/completions" || record.Query != "x=1" ||
		record.Headers["Authorization"][0] != redacted || record.Headers["X-Custom"][0] != "kept" ||
		body.Temperature != 0.7 || body.MaxTokens != 5 || len(body.Messages) != 3 || len(body.Tools) != 1 ||
		!strings.Contains(string(body.Messages[1]), `"tool_calls"`) {
		t.Fatalf("record = %#v body = %#v", record, body)
	}
}

func TestChatCompletionEchoesWholeRequestAndLogsIt(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	server, err := New(Options{LogPath: logPath})
	if err != nil {
		t.Fatal(err)
	}
	response := post(t, server, "false")
	var completion struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct{ Content string }
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &completion); err != nil || response.Code != 200 || len(completion.Choices) != 1 {
		t.Fatalf("response = %d %s %v", response.Code, response.Body, err)
	}
	if completion.Model != "m1" || completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("completion = %#v", completion)
	}
	assertEchoed(t, completion.Choices[0].Message.Content)

	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(logged), "\n") != 1 || strings.Contains(string(logged), "secret-key") {
		t.Fatalf("log = %q, %v", logged, err)
	}
	if info, _ := os.Stat(logPath); info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %v", info.Mode())
	}
}

func TestStreamedChatCompletionEchoesWholeRequest(t *testing.T) {
	server, _ := New(Options{})
	response := post(t, server, "true")
	if response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
	}
	var content strings.Builder
	var finish string
	var usage, done bool
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta        struct{ Content string }
				FinishReason *string `json:"finish_reason"`
			}
			Usage *json.RawMessage
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatal(err)
		}
		usage = usage || chunk.Usage != nil
		for _, choice := range chunk.Choices {
			content.WriteString(choice.Delta.Content)
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
		}
	}
	if !done || !usage || finish != "stop" {
		t.Fatalf("done=%v usage=%v finish=%q", done, usage, finish)
	}
	assertEchoed(t, content.String())
}

func TestModelsAndUnsupportedRoutes(t *testing.T) {
	server, _ := New(Options{Model: "served"})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(response.Body.String(), `"id":"served"`) {
		t.Fatalf("models = %s", response.Body)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader("{}")))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
	response = httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("not json")))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
}
