package execsupervisor

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
)

const (
	ProtocolVersion  = 1
	MaxChunkBytes    = 64 << 10
	MaxMessageBytes  = 16 << 20
	AgentProofPrefix = "\x1eARIES_AGENT_REAPED_"
	AgentProofSuffix = "\x1f"
)

// ExecSpec carries the fields intentionally omitted by core.Command's public
// JSON representation as well as its exact argv and environment.
type ExecSpec struct {
	Path             string            `json:"path"`
	Args             []string          `json:"args,omitempty"`
	Dir              string            `json:"dir,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	User             string            `json:"user,omitempty"`
	TimeoutNS        int64             `json:"timeout_ns,omitempty"`
	OutputLimitBytes int               `json:"output_limit_bytes,omitempty"`
}

// Message is the fixed, private Docker-to-agent-supervisor protocol. Native
// command output is always Data, never interpreted as another protocol frame.
type Message struct {
	Type     string    `json:"type"`
	ID       uint64    `json:"id,omitempty"`
	Exec     *ExecSpec `json:"exec,omitempty"`
	Data     []byte    `json:"data,omitempty"`
	ExitCode *int      `json:"exit_code,omitempty"`
	Error    string    `json:"error,omitempty"`
	Version  int       `json:"version,omitempty"`
}

// ReadMessage reads exactly one bounded frame, leaving any subsequent frame
// untouched. Callers additionally enforce direction and request state.
func ReadMessage(reader io.Reader) (Message, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return Message{}, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > MaxMessageBytes {
		return Message{}, errors.New("invalid agent protocol frame length")
	}
	body := make([]byte, int(length))
	if _, err := io.ReadFull(reader, body); err != nil {
		return Message{}, err
	}
	var message Message
	if err := decodeProtocolJSON(body, &message); err != nil {
		return Message{}, err
	}
	if err := validateMessage(message); err != nil {
		return Message{}, err
	}
	return message, nil
}

// WriteMessage must be serialized by its caller when sharing a writer.
func WriteMessage(writer io.Writer, message Message) error {
	if err := validateMessage(message); err != nil {
		return err
	}
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > MaxMessageBytes {
		return errors.New("invalid agent protocol frame length")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	if err := writeProtocolBytes(writer, header[:]); err != nil {
		return err
	}
	return writeProtocolBytes(writer, body)
}

func writeProtocolBytes(writer io.Writer, data []byte) error {
	written, err := writer.Write(data)
	if err == nil && written != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func decodeProtocolJSON(body []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("invalid agent protocol JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("agent protocol frame contains trailing JSON")
	}
	return nil
}

func validateMessage(message Message) error {
	global := message.Type == "ready" || message.Type == "stop" || message.Type == "fatal"
	if global != (message.ID == 0) {
		return errors.New("agent protocol request identity is invalid")
	}
	wantExec, wantData, wantCode, allowError, wantVersion := false, false, false, false, false
	switch message.Type {
	case "ready":
		wantVersion = true
	case "exec":
		wantExec = true
	case "stdin", "stdout", "stderr":
		wantData = true
	case "exited":
		wantCode, allowError = true, true
	case "stdin_ack", "retired", "fatal":
		allowError = true
	case "stdin_eof", "cancel", "stdout_ack", "stderr_ack", "stdout_eof", "stderr_eof", "stop":
	default:
		return errors.New("unknown agent protocol message")
	}
	if wantExec != (message.Exec != nil) || wantData != (len(message.Data) != 0) || wantCode != (message.ExitCode != nil) || !allowError && message.Error != "" || wantVersion != (message.Version != 0) {
		return errors.New("agent protocol message has unexpected or missing fields")
	}
	if wantVersion && message.Version != ProtocolVersion || len(message.Data) > MaxChunkBytes || message.Type == "fatal" && message.Error == "" {
		return errors.New("invalid agent protocol message value")
	}
	if message.ExitCode != nil && (*message.ExitCode < -1 || *message.ExitCode > 255) {
		return errors.New("invalid native exit code")
	}
	if message.Exec != nil {
		return validateExecSpec(*message.Exec)
	}
	return nil
}

func validateExecSpec(spec ExecSpec) error {
	cleanPath := func(value string) bool {
		return filepath.IsAbs(value) && filepath.Clean(value) == value && !strings.ContainsRune(value, 0)
	}
	if !cleanPath(spec.Path) || spec.Path == "/" || spec.Dir != "" && !cleanPath(spec.Dir) {
		return errors.New("agent command path or workdir is invalid")
	}
	if spec.TimeoutNS < 0 || spec.OutputLimitBytes < 0 || spec.OutputLimitBytes > 1<<30 {
		return errors.New("agent command limit is invalid")
	}
	if spec.User != "" {
		uid, gid, found := strings.Cut(spec.User, ":")
		if !found || !protocolDigits(uid) || !protocolDigits(gid) {
			return errors.New("agent command user must be numeric UID:GID")
		}
	}
	for _, argument := range spec.Args {
		if strings.ContainsRune(argument, 0) {
			return errors.New("agent command argument contains NUL")
		}
	}
	for name, value := range spec.Env {
		if name == "" || strings.ContainsRune(value, 0) {
			return errors.New("agent command environment is invalid")
		}
		for i, r := range name {
			if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
				return errors.New("agent command environment name is invalid")
			}
		}
	}
	return nil
}

func protocolDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
