package echo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

type toolCompletion struct {
	Finish  string
	Content string
	Calls   []struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
}

func requestCompletion(t *testing.T, server http.Handler, stream bool, messages []any, tools []any) toolCompletion {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "mock-tools", "stream": stream, "stream_options": map[string]bool{"include_usage": true}, "messages": messages, "tools": tools})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
	if response.Code != http.StatusOK {
		t.Fatalf("completion status=%d body=%s", response.Code, response.Body)
	}
	var result toolCompletion
	if !stream {
		var completion struct {
			Choices []struct {
				Finish  string `json:"finish_reason"`
				Message struct {
					Content string          `json:"content"`
					Calls   json.RawMessage `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &completion); err != nil {
			t.Fatal(err)
		}
		if len(completion.Choices) != 1 {
			t.Fatalf("choices=%s", response.Body)
		}
		choice := completion.Choices[0]
		result.Finish, result.Content = choice.Finish, choice.Message.Content
		if choice.Message.Calls != nil {
			if err := json.Unmarshal(choice.Message.Calls, &result.Calls); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	if response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("missing event stream content type")
	}
	var done, usage bool
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
				Finish *string `json:"finish_reason"`
				Delta  struct {
					Content string          `json:"content"`
					Calls   json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatal(err)
		}
		usage = usage || chunk.Usage != nil
		for _, choice := range chunk.Choices {
			result.Content += choice.Delta.Content
			if choice.Finish != nil {
				result.Finish = *choice.Finish
			}
			if choice.Delta.Calls != nil {
				if err := json.Unmarshal(choice.Delta.Calls, &result.Calls); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !done || !usage {
		t.Fatalf("stream done=%v usage=%v", done, usage)
	}
	return result
}

func TestToolCommandRoundTrip(t *testing.T) {
	command := `printf '%s\n' 'ARIES_MICROVM'; uname -r; cat /proc/sys/kernel/random/boot_id`
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "exec", "parameters": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]string{"type": "string"}}}}}}
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			server, err := New(Options{ToolCommand: command})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			messages := []any{map[string]any{"role": "user", "content": "Run the sandbox probe."}}
			first := requestCompletion(t, server, stream, messages, tools)
			if first.Finish != "tool_calls" || len(first.Calls) != 1 || first.Content != "" {
				t.Fatalf("first=%#v", first)
			}
			call := first.Calls[0]
			if call.Type != "function" || call.Function.Name != "exec" || call.ID == "" {
				t.Fatalf("tool call=%#v", call)
			}
			// Replay the spelling produced by OpenClaw's native alphanumeric
			// normalization, rather than assuming every emitted ID survives it.
			normalizedID := strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, call.ID)
			if normalizedID != call.ID {
				t.Fatalf("tool ID changes under native normalization: %q -> %q", call.ID, normalizedID)
			}
			if stream && (call.Index == nil || *call.Index != 0) || !stream && call.Index != nil {
				t.Fatalf("stream=%v index=%v", stream, call.Index)
			}
			var args map[string]string
			if err = json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || len(args) != 1 || args["command"] != command {
				t.Fatalf("args=%v error=%v", args, err)
			}
			output := "ARIES_MICROVM\n6.1.102\n5c903735-2118-4751-bb29-2775b608db5d\n"
			messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": normalizedID, "type": call.Type, "function": map[string]string{"name": call.Function.Name, "arguments": call.Function.Arguments}}}}, map[string]any{"role": "tool", "tool_call_id": normalizedID, "content": output})
			second := requestCompletion(t, server, stream, messages, tools)
			if second.Finish != "stop" || len(second.Calls) != 0 || second.Content != "Mock exec tool result:\n"+output {
				t.Fatalf("second=%#v", second)
			}
			// A later user turn starts its own independent single-call cycle.
			messages = append(messages, map[string]string{"role": "user", "content": "Run another probe."})
			third := requestCompletion(t, server, stream, messages, tools)
			if third.Finish != "tool_calls" || len(third.Calls) != 1 {
				t.Fatalf("new turn=%#v", third)
			}
			if third.Calls[0].ID == call.ID {
				t.Fatal("new user turn reused the prior tool ID")
			}
		})
	}
}

func TestToolModeRequiresTheNativeExecTool(t *testing.T) {
	server, err := New(Options{ToolCommand: "uname -r"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, body := range []string{`{"messages":[{"role":"user","content":"hi"}]}`, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"shell"}}]}`} {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "exec function") {
			t.Fatalf("response=%d %s", response.Code, response.Body)
		}
	}
}

func TestFollowupToolCommandReturnsBothResults(t *testing.T) {
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "exec"}}}
	commands := []string{`printf 'first\n'; cat /proc/sys/kernel/random/boot_id`, `printf 'second\n'; cat /proc/sys/kernel/random/boot_id`}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			server, err := New(Options{ToolCommand: commands[0], ToolFollowupCommand: commands[1]})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			initial := []any{map[string]string{"role": "user", "content": "Probe suspend and restore."}}
			messages := append([]any{}, initial...)
			ids := map[string]bool{}
			outputs := []string{"FIRST_BOOT=same-boot\nPID=123", "SECOND_BOOT=same-boot\nPID=123"}
			for step, command := range commands {
				got := requestCompletion(t, server, stream, messages, tools)
				if got.Finish != "tool_calls" || len(got.Calls) != 1 {
					t.Fatalf("step=%d completion=%+v", step, got)
				}
				call := got.Calls[0]
				if ids[call.ID] {
					t.Fatalf("step=%d reused ID %q", step, call.ID)
				}
				for _, r := range call.ID {
					if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
						t.Fatalf("non-alphanumeric call ID %q", call.ID)
					}
				}
				ids[call.ID] = true
				var args map[string]string
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args["command"] != command {
					t.Fatalf("step=%d arguments=%v error=%v", step, args, err)
				}
				replay := requestCompletion(t, server, stream, messages, tools)
				if !reflect.DeepEqual(replay, got) {
					t.Fatalf("replay changed step %d: %+v vs %+v", step, got, replay)
				}
				// Another conversation must still start with its first command.
				parallel := requestCompletion(t, server, stream, initial, tools)
				var firstArgs map[string]string
				if err := json.Unmarshal([]byte(parallel.Calls[0].Function.Arguments), &firstArgs); err != nil || firstArgs["command"] != commands[0] {
					t.Fatalf("parallel chat skipped command: %+v %v", parallel, err)
				}
				messages = append(messages, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": call.ID, "type": "function", "function": map[string]string{"name": "exec", "arguments": call.Function.Arguments}}}}, map[string]string{"role": "tool", "tool_call_id": call.ID, "content": outputs[step]})
			}
			final := requestCompletion(t, server, stream, messages, tools)
			want := "Mock exec tool result:\n" + outputs[0] + "\nMock followup exec tool result:\n" + outputs[1]
			if final.Finish != "stop" || len(final.Calls) != 0 || final.Content != want {
				t.Fatalf("final=%+v", final)
			}
			messages = append(messages, map[string]string{"role": "user", "content": "Repeat probe."})
			next := requestCompletion(t, server, stream, messages, tools)
			if next.Finish != "tool_calls" || len(next.Calls) != 1 || ids[next.Calls[0].ID] {
				t.Fatalf("next turn=%+v", next)
			}
		})
	}
}

func TestFollowupToolCommandValidation(t *testing.T) {
	for _, options := range []Options{{ToolFollowupCommand: "uname -r"}, {ToolCommand: "uname -r", ToolFollowupCommand: "bad\x00command"}} {
		if server, err := New(options); err == nil {
			server.Close()
			t.Fatalf("accepted invalid options %+v", options)
		}
	}
}

func TestToolYieldMillisecondsIsOptionalNativeNumericArgument(t *testing.T) {
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "exec"}}}
	for _, stream := range []bool{false, true} {
		for _, yieldMS := range []int{0, 300000} {
			t.Run(fmt.Sprintf("stream=%v/yield=%d", stream, yieldMS), func(t *testing.T) {
				server, err := New(Options{ToolCommand: "first", ToolFollowupCommand: "second", ToolYieldMS: yieldMS})
				if err != nil {
					t.Fatal(err)
				}
				defer server.Close()
				messages := []any{map[string]string{"role": "user", "content": "probe"}}
				for _, command := range []string{"first", "second"} {
					completion := requestCompletion(t, server, stream, messages, tools)
					if len(completion.Calls) != 1 {
						t.Fatalf("completion=%+v", completion)
					}
					call := completion.Calls[0]
					var args map[string]json.RawMessage
					if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
						t.Fatal(err)
					}
					var gotCommand string
					if err := json.Unmarshal(args["command"], &gotCommand); err != nil || gotCommand != command {
						t.Fatalf("command=%q error=%v", gotCommand, err)
					}
					if yieldMS == 0 {
						if _, ok := args["yieldMs"]; ok || len(args) != 1 {
							t.Fatalf("native default overwritten: %v", args)
						}
					} else {
						var gotYield int
						if err := json.Unmarshal(args["yieldMs"], &gotYield); err != nil || gotYield != yieldMS || len(args) != 2 {
							t.Fatalf("yieldMs must be numeric: %v error=%v", args, err)
						}
					}
					messages = append(messages, map[string]string{"role": "tool", "tool_call_id": call.ID, "content": "completed"})
				}
			})
		}
	}
}

func TestToolYieldMillisecondsValidation(t *testing.T) {
	for _, options := range []Options{{ToolYieldMS: 1}, {ToolCommand: "probe", ToolYieldMS: -1}} {
		if server, err := New(options); err == nil {
			server.Close()
			t.Fatalf("accepted invalid options %+v", options)
		}
	}
}
