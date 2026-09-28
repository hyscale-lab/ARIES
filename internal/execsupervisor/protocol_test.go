package execsupervisor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
)

func TestProtocolPreservesExecIdentityAndBinaryStreams(t *testing.T) {
	code := 0
	messages := []Message{
		{Type: "ready", Version: ProtocolVersion},
		{Type: "exec", ID: 7, Exec: &ExecSpec{Path: "/bin/tool", Args: []string{"", "a b", "x\ny", "$(literal)"}, Dir: "/work", Env: map[string]string{"VALUE": "quoted\nvalue"}, User: "65532:65533", TimeoutNS: 7000, OutputLimitBytes: 12345}},
		{Type: "stdin", ID: 7, Data: []byte{0, 255, '\n', '\r', 1}},
		{Type: "stdout", ID: 7, Data: []byte("ordinary output")},
		{Type: "exited", ID: 7, ExitCode: &code},
		{Type: "stdout_eof", ID: 7},
		{Type: "stderr_eof", ID: 7},
		{Type: "retired", ID: 7},
	}
	var stream bytes.Buffer
	for _, message := range messages {
		if err := WriteMessage(&stream, message); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range messages {
		got, err := ReadMessage(&stream)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("message = %#v, %v; want %#v", got, err, want)
		}
	}
	if _, err := ReadMessage(&stream); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal read = %v", err)
	}
}

func TestProtocolRejectsMalformedOrAmbiguousFrames(t *testing.T) {
	frame := func(body string) []byte {
		encoded := make([]byte, 4, 4+len(body))
		binary.BigEndian.PutUint32(encoded, uint32(len(body)))
		return append(encoded, body...)
	}
	oversize := make([]byte, 4)
	binary.BigEndian.PutUint32(oversize, MaxMessageBytes+1)
	for name, content := range map[string][]byte{
		"empty": {0, 0, 0, 0}, "oversize": oversize,
		"short header": {0, 0}, "short body": {0, 0, 0, 2, '{'},
		"unknown field":      frame(`{"type":"stop","secret":"unexpected"}`),
		"two values":         frame(`{"type":"stop"}{"type":"stop"}`),
		"unknown operation":  frame(`{"type":"eval"}`),
		"invalid command id": frame(`{"type":"stdin","data":"eA=="}`),
		"unexpected payload": frame(`{"type":"stop","data":"eA=="}`),
		"missing zero exit":  frame(`{"type":"exited","id":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadMessage(bytes.NewReader(content)); err == nil {
				t.Fatal("invalid frame was accepted")
			}
		})
	}
	if err := WriteMessage(io.Discard, Message{Type: "stdin", ID: 1, Data: make([]byte, MaxChunkBytes+1)}); err == nil {
		t.Fatal("oversize stream chunk was accepted")
	}
}

func TestProtocolRejectsUnsafeExecFields(t *testing.T) {
	for _, spec := range []ExecSpec{
		{Path: "relative"}, {Path: "/bin/../bin/sh"}, {Path: "/bin/sh", Dir: "relative"},
		{Path: "/bin/sh", Args: []string{"nul\x00arg"}},
		{Path: "/bin/sh", Env: map[string]string{"BAD=NAME": "x"}},
		{Path: "/bin/sh", Env: map[string]string{"NAME": "nul\x00value"}},
		{Path: "/bin/sh", User: "root"}, {Path: "/bin/sh", User: "0:0:0"},
		{Path: "/bin/sh", TimeoutNS: -1}, {Path: "/bin/sh", OutputLimitBytes: -1},
	} {
		if err := WriteMessage(io.Discard, Message{Type: "exec", ID: 1, Exec: &spec}); err == nil {
			t.Errorf("invalid spec was accepted: %#v", spec)
		}
	}
}

type protocolShortWriter struct{}

func (protocolShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestProtocolShortWriteFails(t *testing.T) {
	if err := WriteMessage(protocolShortWriter{}, Message{Type: "stop"}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
}
