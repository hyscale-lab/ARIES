package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func chatBody(instruction string, assistantTurns, tools int) string {
	messages := []map[string]any{
		{"role": "system", "content": "You are an agent."},
		{"role": "user", "content": instruction},
	}
	for turn := 0; turn < assistantTurns; turn++ {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": fmt.Sprintf("step %d", turn)},
			map[string]any{"role": "tool", "content": fmt.Sprintf("output at %d", time.Now().UnixNano())})
	}
	toolList := make([]map[string]any, tools)
	for index := range toolList {
		toolList[index] = map[string]any{"type": "function", "function": map[string]any{"name": fmt.Sprintf("tool%d", index)}}
	}
	body, _ := json.Marshal(map[string]any{"model": "deepseek-flash", "stream": true, "messages": messages, "tools": toolList})
	return string(body)
}

func TestKeyOfFoldsDigitsAndCountsAssistantTurns(t *testing.T) {
	first, err := KeyOf([]byte(chatBody("Fix the repo. Now is 2026-10-25 10:00", 2, 9)))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := KeyOf([]byte(chatBody("Fix the repo. Now is 2026-10-26 11:31", 2, 9)))
	if first != second {
		t.Fatalf("keys differ only by a timestamp: %v vs %v", first, second)
	}
	if first.Turn != 2 || first.Tools != 9 {
		t.Fatalf("key = %+v", first)
	}
	other, _ := KeyOf([]byte(chatBody("Write a report", 2, 9)))
	auxiliary, _ := KeyOf([]byte(chatBody("Fix the repo. Now is 2026-10-25 10:00", 2, 0)))
	if other == first || auxiliary == first {
		t.Fatal("distinct conversations or tool sets share a key")
	}
	parts, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "Write a report"}}}}})
	fromParts, err := KeyOf(parts)
	if err != nil || fromParts.Conversation != other.Conversation {
		t.Fatalf("content parts key = %+v, %v", fromParts, err)
	}
	if _, err := KeyOf([]byte(`{"messages":[{"role":"system","content":"x"}]}`)); err == nil {
		t.Fatal("a request without a user message has a key")
	}
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Sleep(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var lines []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestRecordThenReplayServesTheSameReply(t *testing.T) {
	const reply = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		half := len(reply) / 2
		_, _ = io.WriteString(w, reply[:half])
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, reply[half:])
	}))
	defer upstream.Close()
	directory := t.TempDir()
	store := filepath.Join(directory, "recordings.jsonl")
	recordLog := filepath.Join(directory, "record-requests.jsonl")
	recorder, err := New(Options{Mode: ModeRecord, Upstream: upstream.URL, StorePath: store, LogPath: recordLog})
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(recorder)
	request, _ := http.NewRequest(http.MethodPost, proxy.URL+chatPath, strings.NewReader(chatBody("Fix the repo at 10:00", 1, 3)))
	request.Header.Set("Authorization", "Bearer secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	recorded, _ := io.ReadAll(response.Body)
	response.Body.Close()
	proxy.Close()
	recorder.Close()
	if string(recorded) != reply || gotAuth != "Bearer secret" || gotPath != chatPath {
		t.Fatalf("proxied reply %q, auth %q, path %q", recorded, gotAuth, gotPath)
	}
	stored, _ := os.ReadFile(store)
	if strings.Contains(string(stored), "secret") {
		t.Fatal("the recordings hold the API key")
	}
	if timings := readLines(t, recordLog); len(timings) != 1 || timings[0]["status"].(float64) != 200 || timings[0]["key"] == nil {
		t.Fatalf("record timings = %v", timings)
	}

	clock := &fakeClock{now: time.Unix(1000, 0)}
	replayLog := filepath.Join(directory, "replay-requests.jsonl")
	replayer, err := New(Options{Mode: ModeReplay, StorePath: store, LogPath: replayLog, Model: "deepseek-flash", Now: clock.Now, Sleep: clock.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	defer replayer.Close()
	if replayer.Recordings() != 1 {
		t.Fatalf("recordings = %d", replayer.Recordings())
	}
	server := httptest.NewServer(replayer)
	defer server.Close()
	// Different tool output and a different time: the same point in the task.
	response, err = http.Post(server.URL+chatPath, "application/json", strings.NewReader(chatBody("Fix the repo at 11:45", 1, 3)))
	if err != nil {
		t.Fatal(err)
	}
	replayed, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(replayed) != reply || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("replayed %q (%s), want %q", replayed, response.Header.Get("Content-Type"), reply)
	}
	response, _ = http.Post(server.URL+chatPath, "application/json", strings.NewReader(chatBody("Fix the repo at 11:45", 2, 3)))
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("an unrecorded turn returned %d", response.StatusCode)
	}
	response, _ = http.Get(server.URL + "/v1/models")
	models, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !strings.Contains(string(models), "deepseek-flash") {
		t.Fatalf("models = %s", models)
	}
	timings := readLines(t, replayLog)
	if len(timings) != 3 || timings[0]["hit"] != true || timings[1]["hit"] != false {
		t.Fatalf("replay timings = %v", timings)
	}
}

func TestReplayPacesChunksAtTheirRecordedOffsets(t *testing.T) {
	key, _ := KeyOf([]byte(chatBody("task", 0, 1)))
	recording := Recording{Key: key, Status: http.StatusOK, ContentType: "text/event-stream", Chunks: []Chunk{
		{OffsetMS: 800, Data: []byte("a")}, {OffsetMS: 1300, Data: []byte("b")},
	}}
	line, _ := json.Marshal(recording)
	failed, _ := json.Marshal(Recording{Key: key, Status: http.StatusTooManyRequests, Chunks: []Chunk{{Data: []byte("busy")}}})
	store := filepath.Join(t.TempDir(), "recordings.jsonl")
	// A failed attempt recorded first is skipped in favour of the success.
	if err := os.WriteFile(store, []byte(string(failed)+"\n"+string(line)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		pace string
		want float64
	}{{PaceRecorded, 1300}, {PaceNone, 0}} {
		clock := &fakeClock{now: time.Unix(1000, 0)}
		logPath := filepath.Join(t.TempDir(), "requests.jsonl")
		server, err := New(Options{Mode: ModeReplay, StorePath: store, LogPath: logPath, Pace: test.pace, Now: clock.Now, Sleep: clock.Sleep})
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(chatBody("task", 0, 1))))
		server.Close()
		if recorder.Body.String() != "ab" {
			t.Fatalf("%s: body %q", test.pace, recorder.Body.String())
		}
		if got := readLines(t, logPath)[0]["duration_ms"].(float64); got != test.want {
			t.Fatalf("%s: duration %v ms, want %v", test.pace, got, test.want)
		}
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	store := filepath.Join(t.TempDir(), "recordings.jsonl")
	for _, options := range []Options{
		{Mode: "both", StorePath: store},
		{Mode: ModeRecord, StorePath: store, Upstream: "ftp://x"},
		{Mode: ModeReplay, StorePath: store},
		{Mode: ModeReplay, StorePath: store, Pace: "fast"},
		{Mode: ModeRecord, Upstream: "https://api.deepseek.com"},
	} {
		if server, err := New(options); err == nil {
			server.Close()
			t.Fatalf("New(%+v) succeeded", options)
		}
	}
}

// A client may close the connection as soon as it has the whole reply, while
// the upstream keeps the stream open. That reply is complete and must be
// stored; one the client abandoned before the end marker must not be.
func TestRecordKeepsACompleteReplyTheClientClosedEarly(t *testing.T) {
	for _, test := range []struct {
		name   string
		reply  string
		stored bool
	}{
		{"complete", "data: {\"choices\":[]}\n\ndata: [DONE]\n\n", true},
		{"cut short", "data: {\"choices\":[]}\n\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.reply)
				w.(http.Flusher).Flush()
				// Hold the stream open until the client has gone.
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer upstream.Close()
			defer close(release)
			store := filepath.Join(t.TempDir(), "recordings.jsonl")
			logPath := filepath.Join(t.TempDir(), "requests.jsonl")
			recorder, err := New(Options{Mode: ModeRecord, Upstream: upstream.URL, StorePath: store, LogPath: logPath})
			if err != nil {
				t.Fatal(err)
			}
			proxy := httptest.NewServer(recorder)
			response, err := http.Post(proxy.URL+chatPath, "application/json", strings.NewReader(chatBody("task", 0, 1)))
			if err != nil {
				t.Fatal(err)
			}
			buffer := make([]byte, len(test.reply))
			if _, err := io.ReadFull(response.Body, buffer); err != nil {
				t.Fatal(err)
			}
			// Hang up without waiting for the upstream to end the stream.
			response.Body.Close()
			proxy.CloseClientConnections()
			deadline := time.Now().Add(5 * time.Second)
			for len(readLines(t, logPath)) == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			proxy.Close()
			recorder.Close()
			content, _ := os.ReadFile(store)
			if stored := len(strings.TrimSpace(string(content))) > 0; stored != test.stored {
				t.Fatalf("stored = %v, want %v; log %v", stored, test.stored, readLines(t, logPath))
			}
		})
	}
}
