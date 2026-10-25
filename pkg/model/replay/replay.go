// Package replay records an OpenAI-compatible model's responses and serves them
// back, so runs on different tool sandboxes see the same model output and the
// same model latency. Only the sandbox under test then varies between runs.
//
// In record mode the server forwards every request to an upstream endpoint
// unchanged, returns the upstream reply as it arrives, and stores it with the
// time each part arrived. In replay mode it answers a chat completion from the
// store without contacting any model.
//
// A response is found again by the conversation it belongs to and how far that
// conversation has progressed, not by the full request: tool output can differ
// slightly between sandboxes, and the reply recorded for that point is wanted
// regardless. See Key.
//
// Both modes log each request's timing, which is the model's share of a task's
// latency.
package replay

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	ModeRecord = "record"
	ModeReplay = "replay"

	// PaceRecorded replays each part of a response at the offset it arrived
	// at when recorded, so the model's latency is reproduced. PaceNone sends
	// the whole response at once.
	PaceRecorded = "recorded"
	PaceNone     = "none"

	maxRequestBytes = 32 << 20
	chatPath        = "/v1/chat/completions"
)

// Options configures a Server.
type Options struct {
	Mode string
	// Upstream is the base URL requests are forwarded to in record mode, such
	// as https://api.deepseek.com. The request path is appended unchanged.
	Upstream string
	// StorePath is the recordings file: appended to in record mode, read in
	// replay mode.
	StorePath string
	// LogPath receives one timing line per request. Empty disables it.
	LogPath string
	Pace    string
	// Model is the id /v1/models reports in replay mode.
	Model  string
	Client *http.Client
	Now    func() time.Time
	Sleep  func(time.Duration)
}

// Key identifies a point in one conversation.
type Key struct {
	// Conversation hashes the first user message, which for an agent harness
	// is the task instruction. Digit runs are folded first so a timestamp the
	// harness writes into it does not split one task into many.
	Conversation string `json:"conversation"`
	// Turn counts the assistant messages already in the request.
	Turn int `json:"turn"`
	// Tools is how many tools the request offers, which separates a harness's
	// auxiliary calls, such as a title request, from the agent's own turn.
	Tools int `json:"tools"`
}

func (key Key) String() string {
	return fmt.Sprintf("%s/%d/%d", key.Conversation, key.Turn, key.Tools)
}

// Chunk is one read of the response body and when it arrived, measured from
// the moment the request was received.
type Chunk struct {
	OffsetMS float64 `json:"offset_ms"`
	Data     []byte  `json:"data"`
}

// Recording is one stored response.
type Recording struct {
	Key         Key       `json:"key"`
	RecordedAt  time.Time `json:"recorded_at"`
	Status      int       `json:"status"`
	ContentType string    `json:"content_type"`
	Chunks      []Chunk   `json:"chunks"`
}

// Timing is one line of the request log.
type Timing struct {
	Mode         string    `json:"mode"`
	Path         string    `json:"path"`
	Key          *Key      `json:"key,omitempty"`
	ReceivedAt   time.Time `json:"received_at"`
	FirstByteMS  float64   `json:"first_byte_ms"`
	DurationMS   float64   `json:"duration_ms"`
	Status       int       `json:"status"`
	RequestBytes int       `json:"request_bytes"`
	ReplyBytes   int       `json:"reply_bytes"`
	// Hit is false when replay had no recording for the request.
	Hit   *bool  `json:"hit,omitempty"`
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"`
}

// Server is an http.Handler. Close releases its files.
type Server struct {
	options  Options
	upstream *url.URL

	mu         sync.Mutex
	recordings map[string]Recording
	store      *os.File
	log        *os.File
}

// New opens the store and log for the given mode.
func New(options Options) (*Server, error) {
	if options.Pace == "" {
		options.Pace = PaceRecorded
	}
	if options.Pace != PaceRecorded && options.Pace != PaceNone {
		return nil, fmt.Errorf("pace must be %q or %q", PaceRecorded, PaceNone)
	}
	if options.StorePath == "" {
		return nil, errors.New("a recordings file is required")
	}
	if options.Client == nil {
		options.Client = &http.Client{}
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = time.Sleep
	}
	server := &Server{options: options, recordings: map[string]Recording{}}
	switch options.Mode {
	case ModeRecord:
		upstream, err := url.Parse(options.Upstream)
		if err != nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" {
			return nil, errors.New("record mode needs an http or https upstream URL")
		}
		server.upstream = upstream
		store, err := os.OpenFile(options.StorePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("open recordings: %w", err)
		}
		server.store = store
	case ModeReplay:
		if err := server.load(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("mode must be %q or %q", ModeRecord, ModeReplay)
	}
	if options.LogPath != "" {
		log, err := os.OpenFile(options.LogPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			server.Close()
			return nil, fmt.Errorf("open request log: %w", err)
		}
		server.log = log
	}
	return server, nil
}

// load reads the store. The first successful recording of each key wins: a
// later one for the same key is the harness retrying, or a repeated run.
func (server *Server) load() error {
	file, err := os.Open(server.options.StorePath)
	if err != nil {
		return fmt.Errorf("open recordings: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), maxRequestBytes*2)
	line := 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var recording Recording
		if err := json.Unmarshal(scanner.Bytes(), &recording); err != nil {
			return fmt.Errorf("recordings line %d: %w", line, err)
		}
		if recording.Status != http.StatusOK {
			continue
		}
		if _, seen := server.recordings[recording.Key.String()]; !seen {
			server.recordings[recording.Key.String()] = recording
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read recordings: %w", err)
	}
	return nil
}

// Recordings reports how many keys replay can answer.
func (server *Server) Recordings() int {
	server.mu.Lock()
	defer server.mu.Unlock()
	return len(server.recordings)
}

func (server *Server) Close() error {
	server.mu.Lock()
	defer server.mu.Unlock()
	var errs []error
	for _, file := range []**os.File{&server.store, &server.log} {
		if *file != nil {
			errs = append(errs, (*file).Close())
			*file = nil
		}
	}
	return errors.Join(errs...)
}

func (server *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	received := server.options.Now()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	timing := Timing{Mode: server.options.Mode, Path: r.URL.Path, ReceivedAt: received.UTC(), RequestBytes: len(body)}
	if err != nil {
		timing.Status, timing.Error = http.StatusRequestEntityTooLarge, "read request: "+err.Error()
		writeError(w, timing.Status, timing.Error)
		server.writeTiming(timing)
		return
	}
	var key *Key
	if r.Method == http.MethodPost && r.URL.Path == chatPath {
		if computed, err := KeyOf(body); err == nil {
			key = &computed
		} else {
			timing.Error = err.Error()
		}
	}
	timing.Key = key
	if server.options.Mode == ModeRecord {
		server.forward(w, r, body, received, key, &timing)
	} else {
		server.replay(w, r, received, key, &timing)
	}
	server.writeTiming(timing)
}

// forward relays the request upstream and the reply back as it arrives,
// recording a chat completion's reply.
func (server *Server) forward(w http.ResponseWriter, r *http.Request, body []byte, received time.Time, key *Key, timing *Timing) {
	target := *server.upstream
	target.Path = strings.TrimSuffix(target.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		timing.Status, timing.Error = http.StatusBadGateway, err.Error()
		writeError(w, timing.Status, timing.Error)
		return
	}
	for name, values := range r.Header {
		switch strings.ToLower(name) {
		// The reply is stored as sent, so it must not be compressed.
		case "host", "content-length", "accept-encoding", "connection", "keep-alive", "transfer-encoding", "upgrade", "proxy-connection":
			continue
		}
		request.Header[name] = append([]string(nil), values...)
	}
	response, err := server.options.Client.Do(request)
	if err != nil {
		timing.Status, timing.Error = http.StatusBadGateway, "upstream: "+err.Error()
		writeError(w, timing.Status, timing.Error)
		return
	}
	defer response.Body.Close()
	for name, values := range response.Header {
		switch strings.ToLower(name) {
		case "content-length", "connection", "transfer-encoding", "content-encoding":
			continue
		}
		w.Header()[name] = values
	}
	w.WriteHeader(response.StatusCode)
	timing.Status = response.StatusCode
	recording := Recording{RecordedAt: received.UTC(), Status: response.StatusCode, ContentType: response.Header.Get("Content-Type")}
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			offset := milliseconds(server.options.Now().Sub(received))
			if timing.ReplyBytes == 0 {
				timing.FirstByteMS = offset
			}
			timing.ReplyBytes += n
			if key != nil {
				recording.Chunks = append(recording.Chunks, Chunk{OffsetMS: offset, Data: append([]byte(nil), buffer[:n]...)})
			}
			if _, err := w.Write(buffer[:n]); err != nil {
				timing.Error = "write reply: " + err.Error()
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				timing.Error = "read upstream: " + readErr.Error()
				// A client may hang up once it has the whole reply, before
				// the upstream ends the stream, which cancels this request.
				// That reply is complete and is kept.
				if r.Context().Err() != nil && complete(recording) {
					timing.Error = ""
					timing.Note = "client closed after the complete reply"
				}
			}
			break
		}
	}
	timing.DurationMS = milliseconds(server.options.Now().Sub(received))
	// A reply cut short is not stored: replaying it would truncate a turn.
	if key != nil && timing.Error == "" {
		recording.Key = *key
		if err := server.appendRecording(recording); err != nil {
			timing.Error = err.Error()
		}
	}
}

func (server *Server) replay(w http.ResponseWriter, r *http.Request, received time.Time, key *Key, timing *Timing) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		timing.Status = http.StatusOK
		model := server.options.Model
		if model == "" {
			model = "aries-replay"
		}
		writeJSON(w, map[string]any{"object": "list", "data": []map[string]any{{"id": model, "object": "model", "created": 0, "owned_by": "aries"}}})
		return
	case key == nil:
		timing.Status = http.StatusBadRequest
		if timing.Error == "" {
			timing.Error = "unsupported route " + r.Method + " " + r.URL.Path
		}
		writeError(w, timing.Status, timing.Error)
		return
	}
	server.mu.Lock()
	recording, found := server.recordings[key.String()]
	server.mu.Unlock()
	timing.Hit = &found
	if !found {
		timing.Status, timing.Error = http.StatusNotFound, "no recording for "+key.String()
		writeError(w, timing.Status, timing.Error)
		return
	}
	if recording.ContentType != "" {
		w.Header().Set("Content-Type", recording.ContentType)
	}
	w.WriteHeader(recording.Status)
	timing.Status = recording.Status
	flusher, _ := w.(http.Flusher)
	for _, chunk := range recording.Chunks {
		if server.options.Pace == PaceRecorded {
			if wait := time.Duration(chunk.OffsetMS*float64(time.Millisecond)) - server.options.Now().Sub(received); wait > 0 {
				server.options.Sleep(wait)
			}
		}
		if timing.ReplyBytes == 0 {
			timing.FirstByteMS = milliseconds(server.options.Now().Sub(received))
		}
		timing.ReplyBytes += len(chunk.Data)
		if _, err := w.Write(chunk.Data); err != nil {
			timing.Error = "write reply: " + err.Error()
			break
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	timing.DurationMS = milliseconds(server.options.Now().Sub(received))
}

// complete reports whether a relayed reply was whole: a stream that reached
// its end marker, or a body that is one complete JSON value.
func complete(recording Recording) bool {
	var body []byte
	for _, chunk := range recording.Chunks {
		body = append(body, chunk.Data...)
	}
	if strings.HasPrefix(recording.ContentType, "text/event-stream") {
		return bytes.Contains(body, []byte("data: [DONE]"))
	}
	return json.Valid(bytes.TrimSpace(body))
}

var digits = regexp.MustCompile(`[0-9]+`)

// KeyOf locates a chat completion request in its conversation.
func KeyOf(body []byte) (Key, error) {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return Key{}, fmt.Errorf("chat completion body is not JSON: %w", err)
	}
	key := Key{Tools: len(request.Tools)}
	first := ""
	for _, message := range request.Messages {
		switch message.Role {
		case "assistant":
			key.Turn++
		case "user":
			if first == "" {
				first = contentText(message.Content)
			}
		}
	}
	if first == "" {
		return Key{}, errors.New("chat completion has no user message")
	}
	sum := sha256.Sum256([]byte(digits.ReplaceAllString(first, "0")))
	key.Conversation = hex.EncodeToString(sum[:8])
	return key, nil
}

// contentText flattens a message's content, which is a string or a list of
// parts, to its text.
func contentText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var builder strings.Builder
		for _, part := range parts {
			builder.WriteString(part.Text)
		}
		return builder.String()
	}
	return string(raw)
}

func (server *Server) appendRecording(recording Recording) error {
	line, err := json.Marshal(recording)
	if err != nil {
		return err
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.store == nil {
		return errors.New("recordings closed")
	}
	if _, err := server.store.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write recording: %w", err)
	}
	return nil
}

func (server *Server) writeTiming(timing Timing) {
	line, err := json.Marshal(timing)
	if err != nil {
		return
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.log != nil {
		_, _ = server.log.Write(append(line, '\n'))
	}
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration.Microseconds()) / 1000
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "aries_replay_error"}})
}
